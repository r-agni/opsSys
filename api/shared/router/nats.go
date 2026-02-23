// Package router — NATS JetStream implementation of MessageRouter.
package router

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSRouter implements MessageRouter backed by NATS JetStream.
type NATSRouter struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// NewNATSRouter connects to NATS and returns a NATSRouter.
// url: NATS connection URL, e.g., "nats://127.0.0.1:4222"
// name: client name shown in NATS monitoring (e.g., "telemetry-ingest")
func NewNATSRouter(url, name string) (*NATSRouter, error) {
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.PingInterval(5*time.Second),
		nats.MaxPingsOutstanding(3),
		nats.ReconnectWait(500*time.Millisecond),
		nats.MaxReconnects(-1), // reconnect forever
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			// Logged by caller's observability stack
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats jetstream init: %w", err)
	}

	return &NATSRouter{nc: nc, js: js}, nil
}

// Publish sends to the NATS subject.
// If opts contains a DeduplicationID, uses JetStream PublishMsg with Nats-Msg-Id header.
// Otherwise uses core NATS publish (no persistence, lowest overhead).
func (r *NATSRouter) Publish(ctx context.Context, subject string, data []byte, opts ...PubOptions) error {
	var opt PubOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if opt.DeduplicationID != "" {
		// JetStream publish with deduplication header
		msg := &nats.Msg{
			Subject: subject,
			Data:    data,
			Header:  make(nats.Header),
		}
		msg.Header.Set("Nats-Msg-Id", opt.DeduplicationID)

		_, err := r.js.PublishMsg(ctx, msg)
		return err
	}

	// Core NATS publish — no persistence, lowest latency, highest throughput
	// Used for high-frequency telemetry subjects
	return r.nc.Publish(subject, data)
}

// Subscribe returns a channel of messages on the given subject.
// For JetStream subjects (Durable set), creates a push consumer.
// For core NATS (Durable empty), creates a standard subscription.
func (r *NATSRouter) Subscribe(ctx context.Context, subject string, opts ...SubOptions) (<-chan *Message, error) {
	var opt SubOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	ch := make(chan *Message, 256)

	if opt.Durable != "" {
		// JetStream durable push consumer
		consumerCfg := jetstream.ConsumerConfig{
			Durable:        opt.Durable,
			FilterSubject:  subject,
			AckPolicy:      jetstream.AckExplicitPolicy,
			AckWait:        coalesce(opt.AckWait, 30*time.Second),
			MaxDeliver:     5,
			DeliverPolicy:  jetstream.DeliverNewPolicy,
		}
		if opt.StartTime != nil {
			consumerCfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
			consumerCfg.OptStartTime = opt.StartTime
		}

		// Stream name is inferred from subject (first token before the dot)
		streamName := streamNameFromSubject(subject)
		consumer, err := r.js.CreateOrUpdateConsumer(ctx, streamName, consumerCfg)
		if err != nil {
			return nil, fmt.Errorf("create JetStream consumer %s: %w", opt.Durable, err)
		}

		go func() {
			defer close(ch)
			iter, err := consumer.Messages()
			if err != nil {
				return
			}
			defer iter.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				default:
					msg, err := iter.Next()
					if err != nil {
						return
					}
					msg.Ack()
					select {
					case ch <- &Message{Subject: msg.Subject(), Data: msg.Data()}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	} else {
		// Core NATS subscription — ephemeral, no replay, lowest latency
		sub, err := r.nc.Subscribe(subject, func(msg *nats.Msg) {
			select {
			case ch <- &Message{Subject: msg.Subject, Data: msg.Data, Reply: msg.Reply}:
			default:
				// Channel full — drop message (backpressure on slow consumers)
			}
		})
		if err != nil {
			return nil, fmt.Errorf("nats subscribe %s: %w", subject, err)
		}

		go func() {
			<-ctx.Done()
			sub.Unsubscribe()
			close(ch)
		}()
	}

	return ch, nil
}

func (r *NATSRouter) Close() error {
	r.nc.Close()
	return nil
}

func coalesce(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// streamNameFromSubject returns the JetStream stream name for a given subject.
// Convention: stream name = first segment of the subject.
// "telemetry.vehicle123.attitude" → "telemetry"
// "command.vehicle123"            → "command"
func streamNameFromSubject(subject string) string {
	for i, c := range subject {
		if c == '.' {
			return subject[:i]
		}
	}
	return subject
}
