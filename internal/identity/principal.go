package identity

import (
	"context"

	"github.com/google/uuid"
)

// PrincipalType says who is calling.
type PrincipalType string

const (
	PrincipalUser   PrincipalType = "user"
	PrincipalDevice PrincipalType = "device"
)

// Principal is the authenticated caller, taken from a verified access token. Only authentication
// middleware creates one; handlers never read a tenant id from a request body.
type Principal struct {
	Type     PrincipalType
	TenantID uuid.UUID
	UserID   uuid.UUID // set for users
	DeviceID uuid.UUID // set for devices
	OutletID uuid.UUID // set for devices: the outlet they are paired to
}

// ID is the user id or the device id, for logs and the audit log.
func (p Principal) ID() uuid.UUID {
	if p.Type == PrincipalDevice {
		return p.DeviceID
	}
	return p.UserID
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type accessKey struct{}

// WithAccess returns a context carrying what the user may do, loaded once per request.
func WithAccess(ctx context.Context, a Access) context.Context {
	return context.WithValue(ctx, accessKey{}, a)
}

// AccessFrom returns the access loaded for this request, if any.
func AccessFrom(ctx context.Context) (Access, bool) {
	a, ok := ctx.Value(accessKey{}).(Access)
	return a, ok
}
