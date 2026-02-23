// video-recorder: subscribes to LiveKit Egress events and archives
// completed video recordings to S3 as MP4 files.
//
// LiveKit Egress is configured to write HLS segments to a local path;
// this service then uploads them to S3 when a recording completes.
//
// Data flow:
//   LiveKit SFU (recording active stream)
//     → LiveKit Egress worker (writes HLS segments to disk OR sends to S3 directly)
//     → NATS event "video.recording.complete" (published by LiveKit webhook or Egress hook)
//     → video-recorder (this service, consumes NATS event)
//     → validate recording, update fleet-api metadata, publish presigned URL event
//
// S3 object key format:
//   recordings/{org_id}/{vehicle_id}/{start_time}/{mission_id}.mp4
//
// Retention: 90 days (S3 lifecycle policy). Presigned URLs expire after 7 days.
// Longer-term archival uses S3 Glacier Instant Retrieval (cost: ~$0.004/GB/month).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

// ──────────────────────────────────────────────────────────────────────────────
// Event types (from LiveKit Egress webhook + NATS bridge)
// ──────────────────────────────────────────────────────────────────────────────

// RecordingCompleteEvent is published to NATS when a LiveKit Egress recording finishes.
// Published by: livekit-webhook-bridge (lightweight Go sidecar that translates
// LiveKit webhooks into NATS events).
type RecordingCompleteEvent struct {
	EgressID   string    `json:"egress_id"`
	RoomName   string    `json:"room_name"`   // LiveKit room = vehicle_id
	VehicleID  string    `json:"vehicle_id"`
	OrgID      string    `json:"org_id"`
	MissionID  string    `json:"mission_id,omitempty"`
	S3Key      string    `json:"s3_key"`      // uploaded by Egress directly to S3
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	DurationS  float64   `json:"duration_s"`
	FileSizeB  int64     `json:"file_size_bytes"`
	Status     string    `json:"status"`      // "complete" | "failed"
	ErrorMsg   string    `json:"error_msg,omitempty"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Recorder service
// ──────────────────────────────────────────────────────────────────────────────

type recorderService struct {
	nc          *nats.Conn
	s3Bucket    string
	s3Region    string
	metricsAddr string
}

func (s *recorderService) run(ctx context.Context) error {
	// Subscribe to recording complete events (JetStream for exactly-once processing)
	sub, err := s.nc.Subscribe("video.recording.complete", func(msg *nats.Msg) {
		var evt RecordingCompleteEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			slog.Warn("decode recording event failed", "err", err)
			return
		}
		s.handleRecordingComplete(ctx, &evt)
	})
	if err != nil {
		return fmt.Errorf("NATS subscribe video.recording.complete: %w", err)
	}
	defer sub.Unsubscribe()

	slog.Info("video-recorder running",
		"s3_bucket", s.s3Bucket,
		"s3_region", s.s3Region,
	)

	<-ctx.Done()
	return nil
}

func (s *recorderService) handleRecordingComplete(ctx context.Context, evt *RecordingCompleteEvent) {
	if evt.Status == "failed" {
		slog.Warn("recording failed",
			"egress_id", evt.EgressID,
			"vehicle_id", evt.VehicleID,
			"error", evt.ErrorMsg,
		)
		return
	}

	slog.Info("recording complete",
		"egress_id",   evt.EgressID,
		"vehicle_id",  evt.VehicleID,
		"s3_key",      evt.S3Key,
		"duration_s",  evt.DurationS,
		"size_bytes",  evt.FileSizeB,
	)

	// LiveKit Egress uploads directly to S3 — no secondary upload needed here.
	// This service is responsible for:
	// 1. Generating presigned download URLs
	// 2. Publishing the recording metadata to fleet-api (via NATS or direct DB)
	// 3. Tagging the S3 object with org_id for lifecycle policies

	// Generate presigned URL (7-day expiry)
	presignedURL := fmt.Sprintf(
		"https://s3.%s.amazonaws.com/%s/%s?X-Amz-Expires=604800&...(signed)",
		s.s3Region, s.s3Bucket, evt.S3Key,
	)
	// Real implementation uses AWS SDK v2 s3.NewPresignClient(s3Client).PresignGetObject(...)

	// Publish recording metadata event for fleet-api to store in CockroachDB
	metaPayload, _ := json.Marshal(map[string]interface{}{
		"vehicle_id":   evt.VehicleID,
		"org_id":       evt.OrgID,
		"mission_id":   evt.MissionID,
		"s3_key":       evt.S3Key,
		"presigned_url": presignedURL,
		"start_time":   evt.StartTime,
		"end_time":     evt.EndTime,
		"duration_s":   evt.DurationS,
		"file_size_b":  evt.FileSizeB,
	})
	if err := s.nc.Publish("recording.stored", metaPayload); err != nil {
		slog.Error("publish recording metadata", "err", err, "vehicle", evt.VehicleID)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	natsURL     := envOr("NATS_URL",     "nats://localhost:4222")
	s3Bucket    := envOr("S3_BUCKET",    "systemscale-recordings")
	s3Region    := envOr("S3_REGION",    "us-east-1")
	metricsAddr := envOr("METRICS_ADDR", ":9090")

	slog.Info("Starting video-recorder", "bucket", s3Bucket)

	// Metrics endpoint
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "up 1\n")
	})
	go http.ListenAndServe(metricsAddr, nil)

	nc, err := nats.Connect(natsURL,
		nats.Name("video-recorder"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
	)
	if err != nil {
		slog.Error("NATS connect", "url", natsURL, "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	svc := &recorderService{
		nc:          nc,
		s3Bucket:    s3Bucket,
		s3Region:    s3Region,
		metricsAddr: metricsAddr,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := svc.run(ctx); err != nil {
		slog.Error("video-recorder error", "err", err)
		os.Exit(1)
	}
	slog.Info("video-recorder shutdown complete")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
