package identity

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Errors the HTTP layer maps to problem+json codes. Validation, conflict and not-found problems
// wrap the kernel errors instead.
var (
	// ErrInvalidCredentials covers an unknown email and a wrong password alike, so the response
	// does not reveal which accounts exist.
	ErrInvalidCredentials = errors.New("identity: invalid email or password")
	ErrEmailNotVerified   = errors.New("identity: email address is not verified")
	ErrNoTenant           = errors.New("identity: account does not belong to any business")
	// ErrInvalidToken means a token is malformed, expired, revoked, already used or unknown.
	ErrInvalidToken = errors.New("identity: invalid or expired token")
	// ErrTokenReuse means a refresh token that was already rotated was presented again. The whole
	// session family has been revoked.
	ErrTokenReuse = errors.New("identity: refresh token reuse detected")
	// ErrForbidden means the caller is authenticated but lacks a permission.
	ErrForbidden     = errors.New("identity: forbidden")
	ErrDeviceRevoked = errors.New("identity: device has been revoked")
)

// TenantRequiredError is returned by Login when the account belongs to several businesses and
// the request did not say which.
type TenantRequiredError struct{ TenantIDs []uuid.UUID }

func (e *TenantRequiredError) Error() string {
	return fmt.Sprintf("identity: account belongs to %d businesses; tenant_id is required", len(e.TenantIDs))
}

// ForbiddenError says what the caller may not do: the permission they lack, or a reason (for
// example that they tried to grant a role stronger than their own).
type ForbiddenError struct {
	Permission string
	Reason     string
}

func (e *ForbiddenError) Error() string {
	if e.Reason != "" {
		return "identity: forbidden: " + e.Reason
	}
	return "identity: missing permission " + e.Permission
}

func (e *ForbiddenError) Unwrap() error { return ErrForbidden }
