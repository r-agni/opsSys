// Package router defines the MessageRouter interface — the single boundary between
// all Go services and the message broker (NATS JetStream).
//
// All services use this interface; none import async-nats directly.
// Swapping the message broker requires only implementing this interface.
package router

import (
	"context"
	"time"
)

// Message is a received message from the broker.
type Message struct {
	Subject string
	Data    []byte
	// Reply is set for request-reply patterns (rarely used in this system).
	Reply string
}

// PubOptions controls publish behavior.
type PubOptions struct {
	// DeduplicationID enables exactly-once delivery via JetStream dedup window.
	// Set to the CommandEnvelope.command_id (UUIDv7) for all command publishes.
	DeduplicationID string
	// TTL hints how long the message should be retained. 0 = stream default.
	TTL time.Duration
}

// SubOptions controls subscription behavior.
type SubOptions struct {
	// Durable names the consumer for JetStream durable subscriptions (replay-capable).
	// Empty = ephemeral subscription (core NATS, no persistence, no replay).
	Durable string
	// StartTime requests replay of messages from this time forward (JetStream only).
	// Zero value = live stream only.
	StartTime *time.Time
	// AckWait is how long JetStream waits for Ack() before redelivering.
	AckWait time.Duration
}

// MessageRouter is the interface all services use to publish and subscribe.
// Implementations must be goroutine-safe.
type MessageRouter interface {
	// Publish sends a message to a subject.
	// For high-frequency telemetry: use core NATS subjects (no persistence).
	// For commands/events: use JetStream subjects (persistence + exactly-once).
	Publish(ctx context.Context, subject string, data []byte, opts ...PubOptions) error

	// Subscribe returns a channel of messages matching the subject pattern.
	// Supports NATS wildcards: telemetry.*.attitude (any vehicle's attitude).
	// The returned channel is closed when ctx is cancelled.
	Subscribe(ctx context.Context, subject string, opts ...SubOptions) (<-chan *Message, error)

	// Close cleans up the router's resources.
	Close() error
}

// HandlerFunc is a function that processes one message.
// If it returns an error, the message is nack'd (JetStream) or ignored (core NATS).
type HandlerFunc func(ctx context.Context, msg *Message) error
