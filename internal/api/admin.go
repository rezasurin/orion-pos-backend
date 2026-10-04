package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
)

// errAdminDisabled is returned by operator routes when the server has no platform database.
var errAdminDisabled = errors.New("api: operator console is not enabled")

func (s *Server) platformOrErr() (*platform.Service, error) {
	if s.Platform == nil {
		return nil, errAdminDisabled
	}
	return s.Platform, nil
}

func (s *Server) AdminLogin(ctx context.Context, req openapi.AdminLoginRequestObject) (openapi.AdminLoginResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil || b.Email == "" || b.Password == "" {
		return nil, fmt.Errorf("%w: email and password are required", kernel.ErrValidation)
	}
	if err := allow(s.adminByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := allow(s.adminByAccount, strings.ToLower(strings.TrimSpace(b.Email))); err != nil {
		return nil, err
	}
	ch, err := p.Login(ctx, b.Email, b.Password)
	if err != nil {
		return nil, err
	}
	return openapi.AdminLogin200JSONResponse{ChallengeToken: ch.Token, ChallengeExpiresAt: ch.ExpiresAt, TotpEnrolled: ch.Enrolled}, nil
}

func (s *Server) AdminVerifyTotp(ctx context.Context, req openapi.AdminVerifyTotpRequestObject) (openapi.AdminVerifyTotpResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil || b.ChallengeToken == "" || (deref(b.Code) == "" && deref(b.RecoveryCode) == "") {
		return nil, fmt.Errorf("%w: challenge_token and a code or recovery_code are required", kernel.ErrValidation)
	}
	if err := allow(s.adminByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	// Each challenge allows a handful of guesses, whatever the address.
	sum := sha256.Sum256([]byte(b.ChallengeToken))
	if err := allow(s.adminByChallenge, hex.EncodeToString(sum[:8])); err != nil {
		return nil, err
	}
	sess, err := p.VerifySecondFactor(ctx, b.ChallengeToken, deref(b.Code), deref(b.RecoveryCode),
		platform.Meta{IP: httpserver.ClientIP(ctx), UserAgent: httpserver.UserAgent(ctx)})
	if err != nil {
		return nil, err
	}
	return openapi.AdminVerifyTotp200JSONResponse{
		TokenType: openapi.OperatorSessionTokenTypeBearer, AccessToken: sess.AccessToken,
		AccessExpiresAt: sess.ExpiresAt, OperatorId: sess.OperatorID,
	}, nil
}

func (s *Server) AdminListAuditLog(ctx context.Context, req openapi.AdminListAuditLogRequestObject) (openapi.AdminListAuditLogResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := p.ListAuditLog(ctx, page)
	if err != nil {
		return nil, err
	}
	out := openapi.AdminListAuditLog200JSONResponse{Items: make([]openapi.AuditEntry, len(res.Items))}
	for i, e := range res.Items {
		ip := e.IP
		out.Items[i] = openapi.AuditEntry{
			Id: e.ID, OperatorId: e.OperatorID, Action: e.Action, TargetType: e.TargetType, TargetId: e.TargetID,
			TenantId: e.TenantID, Before: rawOrNil(e.Before), After: rawOrNil(e.After), Reason: e.Reason,
			Ip: &ip, UserAgent: e.UserAgent, CreatedAt: e.CreatedAt,
		}
	}
	if res.Next != uuid.Nil {
		next := res.Next
		out.NextCursor = &next
	}
	return out, nil
}

func rawOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
