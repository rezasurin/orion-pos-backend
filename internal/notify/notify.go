// Package notify sends email. Callers depend on Sender; the provider behind it is a deployment
// choice (BACKEND_PLAN.md section 2). Sending always happens from a river job, never inside a
// request or a database transaction.
package notify

import (
	"context"
	"log/slog"
	"sync"
)

// Message is one plain-text email.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender delivers a message. Implementations must be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// LogSender writes messages to the log instead of sending them. It is for local development,
// where the log is the inbox, and refuses nothing: configuration forbids it in production,
// because a verification link in a log is a credential.
type LogSender struct{ Logger *slog.Logger }

func (s LogSender) Send(ctx context.Context, m Message) error {
	s.Logger.InfoContext(ctx, "email (not sent: log provider)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}

// MemorySender keeps messages in memory. For tests.
type MemorySender struct {
	mu   sync.Mutex
	sent []Message
}

func (s *MemorySender) Send(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

// Sent returns a copy of the messages sent so far.
func (s *MemorySender) Sent() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.sent...)
}
