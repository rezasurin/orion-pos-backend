package sync

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Projector turns events of the types it handles into rows of its module's tables. It is the only
// code that writes those rows from a device's events.
//
// Project runs inside the event's transaction, in a savepoint, and writes only its own module's
// tables. The savepoint is rolled back unless the outcome is Applied, so a projector may validate
// as it goes and does not have to undo partial writes. It must be a pure function of the event,
// the database and env: it may be run again for a parked event, and the transaction may be retried
// after a deadlock.
//
// A projector returns an error only for failures that are not the event's fault. Malformed events
// are Rejected; a sale that looks wrong but whose money has moved is Applied and flagged.
type Projector interface {
	// Handles maps each event type to the highest schema version it understands.
	Handles() map[string]int
	Project(ctx context.Context, tx pgx.Tx, env Env, ev Event) (Outcome, error)
}

type outcomeStatus int

const (
	outcomeNone outcomeStatus = iota
	outcomeApplied
	outcomeRejected
	outcomePending
)

// Outcome is what a projector made of an event. Build one with Applied, Rejected or PendingOn.
type Outcome struct {
	Status outcomeStatus
	// Code and Detail say why an event was rejected.
	Code, Detail string
	// DependsOn is the record a parked event is waiting for.
	DependsOn uuid.UUID
	// Provides lists the records an applied event created, so events parked on them can run.
	Provides []uuid.UUID
}

// Applied says the event was projected. List the ids of the records it created.
func Applied(provides ...uuid.UUID) Outcome {
	return Outcome{Status: outcomeApplied, Provides: provides}
}

// Rejected says the event is malformed and never will be applied.
func Rejected(code, detail string) Outcome {
	return Outcome{Status: outcomeRejected, Code: code, Detail: detail}
}

// PendingOn says the event cannot be applied until the record with this id exists. The server
// keeps it and tries again when an applied event provides that id.
func PendingOn(id uuid.UUID) Outcome { return Outcome{Status: outcomePending, DependsOn: id} }

func (o Outcome) result(id uuid.UUID) Result {
	switch o.Status {
	case outcomeApplied:
		return Result{ID: id, Status: StatusAccepted}
	case outcomePending:
		return Result{ID: id, Status: StatusAccepted, Code: "pending_dependency", Detail: "waiting for " + o.DependsOn.String()}
	}
	return Result{ID: id, Status: StatusRejected, Code: o.Code, Detail: o.Detail}
}
