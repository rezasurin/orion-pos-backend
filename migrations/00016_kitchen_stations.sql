-- Kitchen and bar stations, and which station makes each item (BACKEND_PLAN.md section 6.3.3,
-- task B2.7). The POS prints a ticket per station; which printer a station uses is set on the
-- tablet, since printers belong to an outlet's network. Phase 4 adds tickets with a status for a
-- kitchen display on top of these.

-- +goose Up
CREATE TABLE kitchen_station (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 40),
    sort_order  integer NOT NULL DEFAULT 0,
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);

CREATE UNIQUE INDEX kitchen_station_name_idx ON kitchen_station (tenant_id, lower(name)) WHERE archived_at IS NULL;

-- One station per item; null means no ticket (a bottled drink handed over at the till).
ALTER TABLE item ADD COLUMN station_id uuid;
ALTER TABLE item ADD FOREIGN KEY (tenant_id, station_id) REFERENCES kitchen_station (tenant_id, id);

CREATE TRIGGER kitchen_station_set_updated_at BEFORE UPDATE ON kitchen_station
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE kitchen_station ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON kitchen_station TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON kitchen_station TO orion_platform USING (true) WITH CHECK (true);

-- Archived, never deleted, like the rest of the catalog.
GRANT SELECT, INSERT ON kitchen_station TO orion_app, orion_platform;
GRANT UPDATE (name, sort_order, archived_at) ON kitchen_station TO orion_app;
GRANT UPDATE ON kitchen_station TO orion_platform;
GRANT UPDATE (station_id) ON item TO orion_app;

-- +goose Down
ALTER TABLE item DROP COLUMN station_id;
DROP TABLE kitchen_station;
