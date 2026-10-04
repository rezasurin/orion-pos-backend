package api

import (
	"context"
	"fmt"
	"strings"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/signup"
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

// ForgotPassword always answers 202: nothing in the response, the status or the time it takes
// (beyond one queued job) says whether the address has an account.
func (s *Server) ForgotPassword(ctx context.Context, req openapi.ForgotPasswordRequestObject) (openapi.ForgotPasswordResponseObject, error) {
	if req.Body == nil || req.Body.Email == "" {
		return nil, fmt.Errorf("%w: email is required", kernel.ErrValidation)
	}
	if err := allow(s.forgotByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := allow(s.forgotByEmail, strings.ToLower(strings.TrimSpace(req.Body.Email))); err != nil {
		return nil, err
	}
	if err := s.Identity.RequestPasswordReset(ctx, req.Body.Email); err != nil {
		return nil, err
	}
	return openapi.ForgotPassword202Response{}, nil
}

func (s *Server) ResetPassword(ctx context.Context, req openapi.ResetPasswordRequestObject) (openapi.ResetPasswordResponseObject, error) {
	if req.Body == nil || req.Body.Token == "" || req.Body.Password == "" {
		return nil, fmt.Errorf("%w: token and password are required", kernel.ErrValidation)
	}
	if err := allow(s.resetByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := s.Identity.ResetPassword(ctx, req.Body.Token, req.Body.Password); err != nil {
		return nil, err
	}
	return openapi.ResetPassword204Response{}, nil
}

// SignUp is the public sign-up form. It answers 202 whether the business was created or the
// address already had an account (see signup.Service), and a filled honeypot is accepted and
// ignored: neither lets the caller learn anything.
func (s *Server) SignUp(ctx context.Context, req openapi.SignUpRequestObject) (openapi.SignUpResponseObject, error) {
	b := req.Body
	if b == nil || b.Email == "" {
		return nil, fmt.Errorf("%w: email is required", kernel.ErrValidation)
	}
	if err := allow(s.signupByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := allow(s.signupByEmail, strings.ToLower(strings.TrimSpace(b.Email))); err != nil {
		return nil, err
	}
	if b.Website != nil && strings.TrimSpace(*b.Website) != "" {
		s.Logger.InfoContext(ctx, "sign-up ignored: honeypot filled", "client_ip", httpserver.ClientIP(ctx))
		return openapi.SignUp202Response{}, nil
	}
	in := signup.Input{
		BusinessName: b.BusinessName, OwnerName: b.OwnerName, Email: b.Email, Password: b.Password,
		OutletName: deref(b.OutletName), OutletCode: deref(b.OutletCode),
	}
	if b.Timezone != nil {
		in.Timezone = string(*b.Timezone)
	}
	if b.Locale != nil {
		in.Locale = string(*b.Locale)
	}
	if err := s.Signup.SignUp(ctx, in); err != nil {
		return nil, err
	}
	return openapi.SignUp202Response{}, nil
}

func toSession(s identity.Session) openapi.Session {
	return openapi.Session{
		TokenType:        openapi.SessionTokenTypeBearer,
		AccessToken:      s.AccessToken,
		AccessExpiresAt:  s.AccessExpiresAt,
		RefreshToken:     s.RefreshToken,
		RefreshExpiresAt: s.RefreshExpiresAt,
		TenantId:         s.TenantID,
		UserId:           s.UserID,
	}
}
