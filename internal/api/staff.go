package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

func callerFrom(ctx context.Context) identity.Caller {
	c, ok := identity.CallerFrom(ctx)
	if !ok {
		// authenticate puts the principal and access in place for every user route.
		panic("api: handler ran without an authenticated user")
	}
	return c
}

func (s *Server) ListRoles(ctx context.Context, _ openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	roles, err := s.Identity.ListRoles(ctx, principalFrom(ctx).TenantID)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.Role, len(roles))
	for i, r := range roles {
		perms := make([]string, len(r.Permissions))
		for j, p := range r.Permissions {
			perms[j] = string(p)
		}
		items[i] = openapi.Role{Id: r.ID, Name: r.Name, IsSystem: r.IsSystem, Permissions: perms}
	}
	return openapi.ListRoles200JSONResponse{Items: items}, nil
}

func (s *Server) ListStaff(ctx context.Context, req openapi.ListStaffRequestObject) (openapi.ListStaffResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Identity.ListStaff(ctx, principalFrom(ctx).TenantID, page)
	if err != nil {
		return nil, err
	}
	out := openapi.ListStaff200JSONResponse{Items: make([]openapi.Staff, len(res.Items))}
	for i, st := range res.Items {
		out.Items[i] = toStaff(st)
	}
	if res.Next != uuid.Nil {
		next := res.Next
		out.NextCursor = &next
	}
	return out, nil
}

func (s *Server) CreateStaff(ctx context.Context, req openapi.CreateStaffRequestObject) (openapi.CreateStaffResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, fmt.Errorf("%w: a body is required", kernel.ErrValidation)
	}
	c := callerFrom(ctx)
	in := identity.NewStaff{DisplayName: b.DisplayName, OutletRoles: fromOutletRoles(b.OutletRoles)}
	if b.Pin != nil && *b.Pin != "" {
		if err := allow(s.pinByUser, c.Principal.UserID.String()); err != nil {
			return nil, err
		}
		in.PIN = *b.Pin
	}
	st, err := s.Identity.CreateStaff(ctx, c, in)
	if err != nil {
		return nil, err
	}
	return openapi.CreateStaff201JSONResponse(toStaff(st)), nil
}

func (s *Server) UpdateStaff(ctx context.Context, req openapi.UpdateStaffRequestObject) (openapi.UpdateStaffResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, fmt.Errorf("%w: a body is required", kernel.ErrValidation)
	}
	in := identity.UpdateStaff{DisplayName: b.DisplayName, Active: b.Active}
	if b.OutletRoles != nil {
		roles := fromOutletRoles(b.OutletRoles)
		in.OutletRoles = &roles
	}
	st, err := s.Identity.UpdateStaff(ctx, callerFrom(ctx), req.StaffId, in)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateStaff200JSONResponse(toStaff(st)), nil
}

func (s *Server) SetStaffPin(ctx context.Context, req openapi.SetStaffPinRequestObject) (openapi.SetStaffPinResponseObject, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("%w: a body is required", kernel.ErrValidation)
	}
	c := callerFrom(ctx)
	if err := allow(s.pinByUser, c.Principal.UserID.String()); err != nil {
		return nil, err
	}
	if err := s.Identity.SetPIN(ctx, c, req.StaffId, req.Body.Pin); err != nil {
		return nil, err
	}
	return openapi.SetStaffPin204Response{}, nil
}

// pageFrom reads the cursor and limit query parameters, rejecting a limit outside 1 to 200.
func pageFrom(cursor *openapi.Cursor, limit *openapi.Limit) (kernel.Page, error) {
	var page kernel.Page
	if cursor != nil {
		page.After = *cursor
	}
	if limit != nil {
		if *limit < 1 || *limit > 200 {
			return kernel.Page{}, fmt.Errorf("%w: limit must be between 1 and 200", kernel.ErrValidation)
		}
		page.Limit = *limit
	}
	return page, nil
}

func fromOutletRoles(in *[]openapi.OutletRole) []identity.OutletRole {
	if in == nil {
		return nil
	}
	out := make([]identity.OutletRole, len(*in))
	for i, r := range *in {
		out[i] = identity.OutletRole{OutletID: r.OutletId, RoleID: r.RoleId}
	}
	return out
}

func toStaff(st identity.Staff) openapi.Staff {
	roles := make([]openapi.OutletRole, len(st.OutletRoles))
	for i, r := range st.OutletRoles {
		roles[i] = openapi.OutletRole{OutletId: r.OutletID, RoleId: r.RoleID}
	}
	return openapi.Staff{
		Id: st.ID, UserId: st.UserID, DisplayName: st.DisplayName, Active: st.Active, HasPin: st.HasPIN,
		PinRotatedAt: st.PINRotatedAt, IsOwner: st.IsOwner, OutletRoles: roles, CreatedAt: st.CreatedAt,
	}
}
