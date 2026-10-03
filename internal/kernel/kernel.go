// Package kernel holds the small shared pieces every module uses: ids, money, the clock, the
// tenant context, transactions and error kinds. It must not import any module.
package kernel

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// NewID returns a new UUIDv7. Ids are time-ordered so they also work as pagination cursors.
func NewID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

// Clock is injected wherever time matters, so tests can fix it.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock, in UTC.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock always returns the same instant. For tests.
type FixedClock struct{ T time.Time }

func (c FixedClock) Now() time.Time { return c.T }

type tenantKey struct{}

// WithTenant returns a context that carries the authenticated tenant. Only authentication
// middleware calls it; handlers never take a tenant id from the request body.
func WithTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// TenantFrom returns the tenant in ctx, if any.
func TenantFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(tenantKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}

// Error kinds that modules wrap, so the HTTP layer can map them to problem+json codes.
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrValidation = errors.New("validation failed")
)
