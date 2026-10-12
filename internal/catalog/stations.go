package catalog

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

const maxStationName = 40

// Station is where an item is made, such as "Kitchen" or "Bar". The POS prints a ticket per
// station; which printer that is, is set on the tablet.
type Station struct {
	ID         uuid.UUID
	Name       string
	SortOrder  int
	ArchivedAt *time.Time
	CreatedAt  time.Time
}

// NewStation describes a station to create.
type NewStation struct {
	Name      string
	SortOrder int
}

// UpdateStation changes a station. Nil fields stay as they are.
type UpdateStation struct {
	Name      *string
	SortOrder *int
	Archived  *bool
}

// CreateStation adds a kitchen station.
func (s *Service) CreateStation(ctx context.Context, tenantID uuid.UUID, in NewStation) (Station, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := validName("name", in.Name, maxStationName); err != nil {
		return Station{}, err
	}
	if err := validSort(in.SortOrder); err != nil {
		return Station{}, err
	}
	var out Station
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		row, err := db.New(tx).InsertStation(ctx, db.InsertStationParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: in.Name, SortOrder: int32(in.SortOrder), //nolint:gosec // validated above
		})
		if err != nil {
			return mapErr(err)
		}
		out = toStation(row)
		_, err = kernel.RecordChange(ctx, tx, EntityStation, row.ID, "upsert", nil)
		return err
	})
	return out, err
}

// ListStations returns a page of stations ordered by id.
func (s *Service) ListStations(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[Station], error) {
	var out kernel.Paged[Station]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListStations(ctx, db.ListStationsParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.KitchenStation) uuid.UUID { return r.ID })
		out = kernel.Paged[Station]{Next: paged.Next, Items: make([]Station, len(paged.Items))}
		for i, r := range paged.Items {
			out.Items[i] = toStation(r)
		}
		return nil
	})
	return out, err
}

// UpdateStation applies changes to one station. Archiving one leaves its items pointing at it, so
// old tickets still resolve; the POS prints no ticket for an archived station.
func (s *Service) UpdateStation(ctx context.Context, tenantID, id uuid.UUID, in UpdateStation) (Station, error) {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := validName("name", n, maxStationName); err != nil {
			return Station{}, err
		}
		in.Name = &n
	}
	if in.SortOrder != nil {
		if err := validSort(*in.SortOrder); err != nil {
			return Station{}, err
		}
	}
	var out Station
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetStationForUpdate(ctx, db.GetStationForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateStationParams{TenantID: tenantID, ID: id, Name: cur.Name, SortOrder: cur.SortOrder, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.SortOrder != nil {
			p.SortOrder = int32(*in.SortOrder) //nolint:gosec // validated above
		}
		row, err := q.UpdateStation(ctx, p)
		if err != nil {
			return mapErr(err)
		}
		out = toStation(row)
		_, err = kernel.RecordChange(ctx, tx, EntityStation, id, "upsert", nil)
		return err
	})
	return out, err
}

func toStation(r db.KitchenStation) Station {
	return Station{ID: r.ID, Name: r.Name, SortOrder: int(r.SortOrder), ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
}
