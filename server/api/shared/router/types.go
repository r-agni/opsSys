package router

import (
	"context"
	"time"
)

// MessageRouter abstracts a pub/sub message bus (NATS JetStream, etc.)
// so services depend on an interface rather than a concrete transport.
type MessageRouter interface {
	Publish(ctx context.Context, subject string, data []byte, opts ...PubOptions) error
	Subscribe(ctx context.Context, subject string, opts ...SubOptions) (<-chan *Message, error)
	EnsureStream(ctx context.Context, name string, subjects []string) error
	Close() error
}

// Message represents a single message received from the bus.
type Message struct {
	Subject string
	Data    []byte
	Reply   string
}

// PubOptions configure how a message is published.
type PubOptions struct {
	DeduplicationID string
	TTL             time.Duration
}

// SubOptions configure how a subscription is created.
type SubOptions struct {
	Durable   string
	AckWait   time.Duration
	StartTime *time.Time
}
