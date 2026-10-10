package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform/db"
)

// Announcements are notices from Orion to the back office of every business or of one (task B2.8).
// Title and body are per locale; Indonesian ("id") is required and is shown when the reader's
// locale has none.

// Announcement is one notice.
type Announcement struct {
	ID        uuid.UUID
	TenantID  *uuid.UUID // nil: every business
	Severity  string     // info or warning
	Title     map[string]string
	Body      map[string]string
	StartsAt  time.Time
	EndsAt    *time.Time
	CreatedBy uuid.UUID
	CreatedAt time.Time
}

// NewAnnouncement describes one to publish. StartsAt nil means now; EndsAt nil means until ended.
type NewAnnouncement struct {
	TenantID *uuid.UUID
	Severity string
	Title    map[string]string
	Body     map[string]string
	StartsAt *time.Time
	EndsAt   *time.Time
}

var announcementLocales = map[string]bool{"id": true, "en": true}

// CreateAnnouncement publishes an announcement and audits it.
func (s *Service) CreateAnnouncement(ctx context.Context, a Actor, in NewAnnouncement, reason string) (Announcement, error) {
	if a.OperatorID == nil {
		return Announcement{}, fmt.Errorf("%w: an announcement needs an operator", kernel.ErrValidation)
	}
	if in.Severity != "info" && in.Severity != "warning" {
		return Announcement{}, fmt.Errorf("%w: severity is info or warning", kernel.ErrValidation)
	}
	for field, texts := range map[string]struct {
		m   map[string]string
		max int
	}{"title": {in.Title, 120}, "body": {in.Body, 2000}} {
		for loc, v := range texts.m {
			if !announcementLocales[loc] {
				return Announcement{}, fmt.Errorf("%w: %s has an unknown locale %q (id or en)", kernel.ErrValidation, field, loc)
			}
			v = strings.TrimSpace(v)
			if v == "" || utf8.RuneCountInString(v) > texts.max {
				return Announcement{}, fmt.Errorf("%w: %s.%s is required and at most %d characters", kernel.ErrValidation, field, loc, texts.max)
			}
			texts.m[loc] = v
		}
		if texts.m["id"] == "" {
			return Announcement{}, fmt.Errorf("%w: %s needs Indonesian (id)", kernel.ErrValidation, field)
		}
	}
	now := s.Clock.Now()
	starts := now
	if in.StartsAt != nil {
		starts = *in.StartsAt
	}
	if in.EndsAt != nil && !in.EndsAt.After(starts) {
		return Announcement{}, fmt.Errorf("%w: ends_at must be after starts_at", kernel.ErrValidation)
	}
	title, _ := json.Marshal(in.Title)
	body, _ := json.Marshal(in.Body)
	var out Announcement
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).InsertAnnouncement(ctx, db.InsertAnnouncementParams{
			ID: kernel.NewID(), TenantID: in.TenantID, Severity: in.Severity, Title: title, Body: body,
			StartsAt: starts, EndsAt: in.EndsAt, CreatedBy: *a.OperatorID,
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return fmt.Errorf("%w: no business %s", kernel.ErrNotFound, in.TenantID)
		}
		if err != nil {
			return err
		}
		out = toAnnouncement(row)
		return s.audit(ctx, tx, a, Entry{
			Action: "announcement.created", TargetType: "announcement", TargetID: &out.ID, TenantID: in.TenantID,
			After: map[string]any{"severity": in.Severity, "title": in.Title, "starts_at": starts, "ends_at": in.EndsAt}, Reason: reason,
		})
	})
	return out, err
}

// ListAnnouncements returns the 200 most recent, ended ones included.
func (s *Service) ListAnnouncements(ctx context.Context) ([]Announcement, error) {
	rows, err := db.New(s.Pool).ListAnnouncements(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Announcement, len(rows))
	for i, r := range rows {
		out[i] = toAnnouncement(r)
	}
	return out, nil
}

// EndAnnouncement takes an announcement down now and audits it.
func (s *Service) EndAnnouncement(ctx context.Context, a Actor, id uuid.UUID, reason string) (Announcement, error) {
	var out Announcement
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).EndAnnouncement(ctx, db.EndAnnouncementParams{ID: id, Now: s.Clock.Now()})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no announcement %s that has not ended", kernel.ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		out = toAnnouncement(row)
		return s.audit(ctx, tx, a, Entry{
			Action: "announcement.ended", TargetType: "announcement", TargetID: &id, TenantID: out.TenantID,
			After: map[string]any{"ends_at": out.EndsAt}, Reason: reason,
		})
	})
	return out, err
}

func toAnnouncement(r db.Announcement) Announcement {
	out := Announcement{
		ID: r.ID, TenantID: r.TenantID, Severity: r.Severity, StartsAt: r.StartsAt, EndsAt: r.EndsAt,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}
	_ = json.Unmarshal(r.Title, &out.Title)
	_ = json.Unmarshal(r.Body, &out.Body)
	return out
}
