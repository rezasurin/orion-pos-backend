package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/rezasurin/orion-pos-backend/internal/identity/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
)

// VerifyEmailArgs asks the worker to email a verification link. It carries ids only: the token
// is minted by the worker, so no secret ever sits in the job table.
type VerifyEmailArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
}

func (VerifyEmailArgs) Kind() string { return "verify_email" }

// PurgeTokensArgs deletes expired refresh tokens and verification links.
type PurgeTokensArgs struct{}

func (PurgeTokensArgs) Kind() string { return "purge_expired_tokens" }

// purgeGrace keeps expired tokens for a while so support can tell "expired" from "never existed".
const purgeGrace = 7 * 24 * time.Hour

// JobsDeps are what the worker side of identity needs.
type JobsDeps struct {
	Platform *pgxpool.Pool // a member of orion_platform: jobs span tenants
	Sender   notify.Sender
	Clock    kernel.Clock
	Logger   *slog.Logger
	// PublicURL is the front end's base URL, for links in emails.
	PublicURL       string
	VerificationTTL time.Duration
}

// Jobs runs identity's background jobs. It belongs to `orion worker`.
type Jobs struct{ JobsDeps }

// NewJobs returns the worker side of identity.
func NewJobs(d JobsDeps) *Jobs {
	if d.Clock == nil {
		d.Clock = kernel.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.VerificationTTL == 0 {
		d.VerificationTTL = DefaultConfig().VerificationTTL
	}
	return &Jobs{d}
}

// AddWorkers registers the job handlers.
func (j *Jobs) AddWorkers(w *river.Workers) {
	river.AddWorker(w, &verifyEmailWorker{j: j})
	river.AddWorker(w, &purgeTokensWorker{j: j})
}

// PeriodicJobs are the jobs that run on a schedule.
func (j *Jobs) PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return PurgeTokensArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}
}

type verifyEmailWorker struct {
	river.WorkerDefaults[VerifyEmailArgs]
	j *Jobs
}

func (w *verifyEmailWorker) Work(ctx context.Context, job *river.Job[VerifyEmailArgs]) error {
	return w.j.SendVerificationEmail(ctx, job.Args)
}

// SendVerificationEmail mints a verification link for the user and emails it. The worker calls it
// for each VerifyEmailArgs job; it is exported so a command or a test can run one directly.
func (j *Jobs) SendVerificationEmail(ctx context.Context, args VerifyEmailArgs) error {
	q := db.New(j.Platform)
	u, err := q.GetUserForEmail(ctx, args.UserID)
	if err != nil {
		return mapNoRowsCancel(err)
	}
	if u.EmailVerifiedAt != nil {
		return nil // verified in the meantime
	}

	token, hash := mintOpaque(prefixVerification, args.TenantID)
	if err := q.InsertEmailVerification(ctx, db.InsertEmailVerificationParams{
		ID: kernel.NewID(), TenantID: args.TenantID, UserID: u.ID,
		TokenHash: hash, ExpiresAt: j.Clock.Now().Add(j.VerificationTTL),
	}); err != nil {
		return err
	}
	subject, text := verificationEmail(u.Locale, verificationLink(j.PublicURL, token))
	if err := j.Sender.Send(ctx, notify.Message{To: u.Email, Subject: subject, Text: text}); err != nil {
		return fmt.Errorf("send verification email: %w", err)
	}
	return nil
}

type purgeTokensWorker struct {
	river.WorkerDefaults[PurgeTokensArgs]
	j *Jobs
}

func (w *purgeTokensWorker) Work(ctx context.Context, _ *river.Job[PurgeTokensArgs]) error {
	return w.j.PurgeExpiredTokens(ctx)
}

// PurgeExpiredTokens deletes refresh tokens and verification links that expired more than a week
// ago.
func (j *Jobs) PurgeExpiredTokens(ctx context.Context) error {
	q := db.New(j.Platform)
	before := j.Clock.Now().Add(-purgeGrace)
	tokens, err := q.PurgeRefreshTokens(ctx, before)
	if err != nil {
		return err
	}
	links, err := q.PurgeEmailVerifications(ctx, before)
	if err != nil {
		return err
	}
	j.Logger.InfoContext(ctx, "purged expired tokens", "refresh_tokens", tokens, "verification_links", links)
	return nil
}

// mapNoRowsCancel cancels a job whose user no longer exists: retrying cannot help.
func mapNoRowsCancel(err error) error {
	if mapNoRows(err, nil) == nil {
		return river.JobCancel(err)
	}
	return err
}
