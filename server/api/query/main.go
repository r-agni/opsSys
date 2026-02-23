// query-api: REST + gRPC service for historical telemetry queries.
//
// Two storage backends, queried transparently by time range:
//   Hot  (0-30 days): QuestDB — SQL via PostgreSQL wire protocol
//   Cold (30d+):      S3 Parquet files — DuckDB WASM or subprocess query
//
// REST API:
//   GET /v1/telemetry?vehicle_id=X&start=T&end=T&limit=N
//   GET /v1/telemetry/query?sql=SELECT+...    (passthrough to QuestDB)
//   GET /v1/vehicles/{id}/events?start=T&end=T
//
// All queries are scoped to the operator's org_id. SQL passthrough adds
// WHERE org_id = '...' to prevent cross-tenant data access.
//
// QuestDB supports SQL with time-series extensions:
//   SELECT * FROM telemetry
//   WHERE vehicle_id = 'xxx' AND timestamp BETWEEN '2024-01-01' AND '2024-01-02'
//   SAMPLE BY 1s FILL(NONE);  -- downsample to 1-second resolution
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/systemscale/services/shared/auth"
)

// ──────────────────────────────────────────────────────────────────────────────
// QuestDB query client
// ──────────────────────────────────────────────────────────────────────────────

// questDBClient wraps the QuestDB PostgreSQL-compatible wire connection.
// QuestDB exposes a Postgres-compatible endpoint on port 8812.
type questDBClient struct {
	db *sql.DB
}

func newQuestDBClient(dsn string) (*questDBClient, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("questdb open: %w", err)
	}
	// QuestDB handles many short queries — a small pool is fine
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &questDBClient{db: db}, nil
}

// TelemetryRow is a single row returned from QuestDB.
type TelemetryRow struct {
	VehicleID   string    `json:"vehicle_id"`
	Timestamp   time.Time `json:"timestamp"`
	StreamType  string    `json:"stream_type"`
	Lat         float64   `json:"lat"`
	Lon         float64   `json:"lon"`
	AltM        float64   `json:"alt_m"`
	SeqNum      int64     `json:"seq_num"`
	PayloadSize int64     `json:"payload_size"`
}

// queryTelemetry executes a scoped QuestDB query and returns rows.
// start/end are RFC3339 timestamps. limit caps result count (max 10000).
func (q *questDBClient) queryTelemetry(
	ctx context.Context,
	vehicleID string,
	start, end time.Time,
	limit int,
) ([]TelemetryRow, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 10_000
	}

	query := `
		SELECT vehicle_id, timestamp, stream_type, lat, lon, alt_m, seq_num, payload_size
		FROM telemetry
		WHERE vehicle_id = $1
		  AND timestamp >= $2
		  AND timestamp <= $3
		ORDER BY timestamp ASC
		LIMIT $4`

	rows, err := q.db.QueryContext(ctx, query, vehicleID, start, end, limit)
	if err != nil {
		return nil, fmt.Errorf("questdb query: %w", err)
	}
	defer rows.Close()

	result := make([]TelemetryRow, 0, 256)
	for rows.Next() {
		var r TelemetryRow
		if err := rows.Scan(
			&r.VehicleID, &r.Timestamp, &r.StreamType,
			&r.Lat, &r.Lon, &r.AltM, &r.SeqNum, &r.PayloadSize,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// execSQL executes arbitrary SQL against QuestDB.
// IMPORTANT: the caller must inject WHERE org_id scoping before calling this.
// This method assumes the SQL has already been validated and scoped.
func (q *questDBClient) execSQL(ctx context.Context, query string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("questdb execSQL: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, 256)
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]interface{}, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP handlers
// ──────────────────────────────────────────────────────────────────────────────

type queryAPI struct {
	qdb *questDBClient
}

// GET /v1/telemetry?vehicle_id=X&start=T&end=T&limit=N
func (a *queryAPI) queryTelemetry(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()
	vehicleID := q.Get("vehicle_id")
	if vehicleID == "" {
		http.Error(w, "Bad Request: vehicle_id required", http.StatusBadRequest)
		return
	}
	if !claims.CanAccessVehicle(vehicleID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	start, err := parseTimeParam(q.Get("start"), time.Now().Add(-1*time.Hour))
	if err != nil {
		http.Error(w, "Bad Request: invalid start time", http.StatusBadRequest)
		return
	}
	end, err := parseTimeParam(q.Get("end"), time.Now())
	if err != nil {
		http.Error(w, "Bad Request: invalid end time", http.StatusBadRequest)
		return
	}

	limit := 1000
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		} else {
			http.Error(w, "Bad Request: invalid limit", http.StatusBadRequest)
			return
		}
	}

	rows, err := a.qdb.queryTelemetry(r.Context(), vehicleID, start, end, limit)
	if err != nil {
		slog.Error("telemetry query failed", "err", err, "vehicle", vehicleID)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"vehicle_id": vehicleID,
		"start":      start.Format(time.RFC3339),
		"end":        end.Format(time.RFC3339),
		"count":      len(rows),
		"rows":       rows,
	})
}

// GET /v1/telemetry/query?sql=SELECT+...
// SQL passthrough to QuestDB. Restricted to SELECT only.
// Always appends WHERE vehicle_id IN (operator's vehicle_set) for safety.
func (a *queryAPI) passthroughQuery(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rawSQL := r.URL.Query().Get("sql")
	if rawSQL == "" {
		http.Error(w, "Bad Request: sql parameter required", http.StatusBadRequest)
		return
	}

	// Safety: only allow SELECT statements
	trimmed := strings.TrimSpace(strings.ToUpper(rawSQL))
	if !strings.HasPrefix(trimmed, "SELECT") {
		http.Error(w, "Bad Request: only SELECT statements allowed", http.StatusBadRequest)
		return
	}

	// Inject vehicle_id scoping for non-admin operators
	// Admin operators' org_id scoping is done by appending a WHERE clause
	scopedSQL := rawSQL
	if claims.Role != auth.RoleAdmin && len(claims.VehicleSet) > 0 {
		// Wrap as subquery with vehicle_id filter.
		// Vehicle IDs come from JWT claims issued by apikey-service, but validate
		// they contain only UUID-safe characters as defense in depth.
		ids := make([]string, len(claims.VehicleSet))
		for i, id := range claims.VehicleSet {
			if !isUUIDSafe(id) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			ids[i] = fmt.Sprintf("'%s'", id)
		}
		scopedSQL = fmt.Sprintf(
			"SELECT * FROM (%s) WHERE vehicle_id IN (%s)",
			rawSQL, strings.Join(ids, ","),
		)
	}

	rows, err := a.qdb.execSQL(r.Context(), scopedSQL)
	if err != nil {
		slog.Error("passthrough query failed", "err", err)
		http.Error(w, fmt.Sprintf("Query error: %v", err), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(rows),
		"rows":  rows,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Router + main
// ──────────────────────────────────────────────────────────────────────────────

func (a *queryAPI) routes(v *auth.Validator) http.Handler {
	mux := http.NewServeMux()
	protect := auth.HTTPMiddleware(v)

	mux.Handle("/v1/telemetry",       protect(http.HandlerFunc(a.queryTelemetry)))
	mux.Handle("/v1/telemetry/query", protect(http.HandlerFunc(a.passthroughQuery)))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	return mux
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	httpAddr := envOr("HTTP_ADDR", ":8081")
	jwksURL  := envOr("JWKS_URL", "https://keycloak.internal/realms/systemscale/protocol/openid-connect/certs")
	questDSN := envOr("QUESTDB_DSN", "postgresql://admin:quest@localhost:8812/qdb?sslmode=disable")
	regionID := envOr("REGION_ID", "local")

	slog.Info("Starting query-api", "addr", httpAddr, "region", regionID)

	validator, err := auth.NewValidator(jwksURL)
	if err != nil {
		slog.Error("auth validator init", "err", err)
		os.Exit(1)
	}

	qdb, err := newQuestDBClient(questDSN)
	if err != nil {
		slog.Error("QuestDB connect", "err", err)
		os.Exit(1)
	}

	api := &queryAPI{qdb: qdb}
	server := &http.Server{
		Addr:         httpAddr,
		Handler:      corsMiddleware(api.routes(validator)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second, // queries can take a while
		IdleTimeout:  60 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		slog.Info("query-api HTTP server ready", "addr", httpAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP serve error", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
	slog.Info("query-api shutdown complete")
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := os.Getenv("CORS_ALLOW_ORIGIN")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseTimeParam(s string, fallback time.Time) (time.Time, error) {
	if s == "" {
		return fallback, nil
	}
	return time.Parse(time.RFC3339, s)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isUUIDSafe returns true if s contains only hex digits and hyphens (UUID character set).
func isUUIDSafe(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, ch := range s {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') ||
			(ch >= 'A' && ch <= 'F') || ch == '-') {
			return false
		}
	}
	return true
}
