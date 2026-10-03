package kernel

import "github.com/google/uuid"

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Page is a cursor pagination request (BACKEND_PLAN.md section 4.10). Rows are ordered by id,
// which is a UUIDv7 and so also by creation time, and the cursor is the last id of the previous
// page.
type Page struct {
	After uuid.UUID // zero starts from the beginning
	Limit int       // zero means the default; capped at 200
}

// Size is the number of rows to return.
func (p Page) Size() int {
	switch {
	case p.Limit <= 0:
		return defaultPageSize
	case p.Limit > maxPageSize:
		return maxPageSize
	}
	return p.Limit
}

// Fetch is the number of rows to ask the database for: one more than Size, to learn whether
// another page exists without a second query.
func (p Page) Fetch() int32 { return int32(p.Size()) + 1 } //nolint:gosec // Size is at most 200

// Paged is one page of results.
type Paged[T any] struct {
	Items []T
	// Next is the cursor for the following page; the zero value means this was the last page.
	Next uuid.UUID
}

// Trim cuts rows fetched with Page.Fetch down to one page. ids returns a row's id.
func Trim[T any](p Page, rows []T, id func(T) uuid.UUID) Paged[T] {
	if len(rows) <= p.Size() {
		return Paged[T]{Items: rows}
	}
	rows = rows[:p.Size()]
	return Paged[T]{Items: rows, Next: id(rows[len(rows)-1])}
}
