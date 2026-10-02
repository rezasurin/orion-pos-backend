package api

import (
	"context"
	"fmt"
	"strings"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

func (s *Server) Login(ctx context.Context, req openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	b := req.Body
	if b == nil || b.Email == "" || b.Password == "" {
		return nil, fmt.Errorf("%w: email and password are required", kernel.ErrValidation)
	}
	ip := httpserver.ClientIP(ctx)
	if err := allow(s.loginByIP, ip); err != nil {
		return nil, err
	}
	if err := allow(s.loginByAccount, strings.ToLower(strings.TrimSpace(b.Email))); err != nil {
		return nil, err
	}
	in := identity.LoginInput{Email: b.Email, Password: b.Password}
	if b.TenantId != nil {
		in.TenantID = *b.TenantId
	}
	sess, err := s.Identity.Login(ctx, in)
	if err != nil {
		return nil, err
	}
	return openapi.Login200JSONResponse(toSession(sess)), nil
}

func (s *Server) RefreshSession(ctx context.Context, req openapi.RefreshSessionRequestObject) (openapi.RefreshSessionResponseObject, error) {
	if req.Body == nil || req.Body.RefreshToken == "" {
		return nil, fmt.Errorf("%w: refresh_token is required", kernel.ErrValidation)
	}
	if err := allow(s.tokenByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	sess, err := s.Identity.Refresh(ctx, req.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	return openapi.RefreshSession200JSONResponse(toSession(sess)), nil
}

func (s *Server) Logout(ctx context.Context, req openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	if req.Body == nil || req.Body.RefreshToken == "" {
		return nil, fmt.Errorf("%w: refresh_token is required", kernel.ErrValidation)
	}
	if err := s.Identity.Logout(ctx, req.Body.RefreshToken); err != nil {
		return nil, err
	}
	return openapi.Logout204Response{}, nil
}

func (s *Server) VerifyEmail(ctx context.Context, req openapi.VerifyEmailRequestObject) (openapi.VerifyEmailResponseObject, error) {
	if req.Body == nil || req.Body.Token == "" {
		return nil, fmt.Errorf("%w: token is required", kernel.ErrValidation)
	}
	if err := allow(s.tokenByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := s.Identity.VerifyEmail(ctx, req.Body.Token); err != nil {
		return nil, err
	}
	return openapi.VerifyEmail204Response{}, nil
}

func (s *Server) ResendVerification(ctx context.Context, req openapi.ResendVerificationRequestObject) (openapi.ResendVerificationResponseObject, error) {
	if req.Body == nil || req.Body.Email == "" {
		return nil, fmt.Errorf("%w: email is required", kernel.ErrValidation)
	}
	if err := allow(s.emailByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := allow(s.emailByAccount, strings.ToLower(strings.TrimSpace(req.Body.Email))); err != nil {
		return nil, err
	}
	if err := s.Identity.ResendVerification(ctx, req.Body.Email); err != nil {
		return nil, err
	}
	return openapi.ResendVerification202Response{}, nil
}

func toSession(s identity.Session) openapi.Session {
	return openapi.Session{
		TokenType:        openapi.Bearer,
		AccessToken:      s.AccessToken,
		AccessExpiresAt:  s.AccessExpiresAt,
		RefreshToken:     s.RefreshToken,
		RefreshExpiresAt: s.RefreshExpiresAt,
		TenantId:         s.TenantID,
		UserId:           s.UserID,
	}
}
