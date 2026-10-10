-- Device health (BACKEND_PLAN.md section 4.11, task B1.10): what a tablet says is still waiting in its
-- outbox, and the alerts raised when events stay unsynced too long.

-- +goose Up
-- The device reports its outbox on every push and pull: how many events it has not delivered, and
-- the device time of the oldest. The server cannot see an outbox, so this is how it knows.
ALTER TABLE device
    ADD COLUMN unsynced_events     integer NOT NULL DEFAULT 0 CHECK (unsynced_events >= 0),
    ADD COLUMN oldest_unsynced_at  timestamptz,
    ADD COLUMN health_reported_at  timestamptz,
    ADD CONSTRAINT device_unsynced_consistent CHECK ((unsynced_events = 0) = (oldest_unsynced_at IS NULL));

GRANT UPDATE (unsynced_events, oldest_unsynced_at, health_reported_at) ON device TO orion_app;

-- One row per incident. At most one is open for a device and kind at a time; it is resolved when the
-- condition clears. The monitor (a periodic job, as orion_platform) opens and resolves them and
-- emails the owners once per incident.
CREATE TABLE device_alert (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    device_id   uuid NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('unsynced_events', 'silent_open_shift')),
    opened_at   timestamptz NOT NULL,
    detail      jsonb NOT NULL DEFAULT '{}',
    notified_at timestamptz,
    resolved_at timestamptz,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, device_id) REFERENCES device (tenant_id, id)
);

-- At most one open incident per device and kind; also how the monitor finds the open ones.
CREATE UNIQUE INDEX device_alert_open_idx ON device_alert (tenant_id, device_id, kind) WHERE resolved_at IS NULL;

-- The monitor looks for devices with a shift still open; an index of only the open ones stays tiny
-- however many shifts have closed.
CREATE INDEX shift_open_idx ON shift (device_id) WHERE closed_at IS NULL;

ALTER TABLE device_alert ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON device_alert TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON device_alert TO orion_platform USING (true) WITH CHECK (true);

-- The service only reads alerts (a back-office list); the monitor writes them.
GRANT SELECT ON device_alert TO orion_app;
GRANT SELECT, INSERT, UPDATE ON device_alert TO orion_platform;

-- +goose Down
DROP INDEX shift_open_idx;
DROP TABLE device_alert;
ALTER TABLE device
    DROP CONSTRAINT device_unsynced_consistent,
    DROP COLUMN health_reported_at,
    DROP COLUMN oldest_unsynced_at,
    DROP COLUMN unsynced_events;
