-- The sync inbox: every event a device pushes, with what became of it (BACKEND_PLAN.md section 5.1).
-- Events are immutable; only the outcome columns change, and only while an event is parked.

-- +goose Up
CREATE TABLE sync_inbox (
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    device_id       uuid NOT NULL,
    idempotency_key uuid NOT NULL,
    -- The event's own id (client-generated UUIDv7). Unique per tenant so two devices cannot push
    -- the same record id; for create events it is also the id of the record created.
    id              uuid NOT NULL,
    outlet_id       uuid NOT NULL,
    staff_id        uuid NOT NULL, -- no foreign key: an event naming an unknown person is kept, rejected
    event_type      text NOT NULL CHECK (event_type ~ '^[a-z_]+(\.[a-z_]+)+$'),
    schema_version  integer NOT NULL CHECK (schema_version >= 1),
    device_time     timestamptz NOT NULL,
    -- SHA-256 of the canonical envelope (type, staff, device time, schema version, payload). The
    -- same key with another hash is a client bug and is rejected as idempotency_conflict.
    payload_hash    bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    payload         jsonb NOT NULL,
    -- 'received' only exists inside the transaction that is projecting the event.
    status          text NOT NULL CHECK (status IN ('received', 'accepted', 'rejected', 'pending_dependency')),
    code            text,          -- why it was rejected, or 'pending_dependency'
    detail          text,
    depends_on      uuid,          -- the record a parked event is waiting for
    received_at     timestamptz NOT NULL,
    applied_at      timestamptz,
    PRIMARY KEY (tenant_id, device_id, idempotency_key),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, device_id) REFERENCES device (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    CHECK ((status = 'pending_dependency') = (depends_on IS NOT NULL)),
    CHECK (status <> 'rejected' OR code IS NOT NULL)
);

-- Finding the parked events a newly applied record unblocks. Partial, so it stays as small as the
-- number of events currently waiting, which should be close to none.
CREATE INDEX sync_inbox_pending_idx ON sync_inbox (tenant_id, depends_on) WHERE status = 'pending_dependency';

-- An event's outcome is settled once it is accepted or rejected. Only a new or parked event may move on.
-- +goose StatementBegin
CREATE FUNCTION sync_inbox_settled() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.status NOT IN ('received', 'pending_dependency') THEN
        RAISE EXCEPTION 'sync_inbox: the outcome of a settled event cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER sync_inbox_settled BEFORE UPDATE ON sync_inbox
    FOR EACH ROW EXECUTE FUNCTION sync_inbox_settled();

ALTER TABLE sync_inbox ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sync_inbox TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON sync_inbox TO orion_platform USING (true) WITH CHECK (true);

-- Events are never rewritten or deleted by the service. Only a parked event's outcome can change.
GRANT SELECT, INSERT ON sync_inbox TO orion_app, orion_platform;
GRANT UPDATE (status, code, detail, depends_on, applied_at) ON sync_inbox TO orion_app;
GRANT UPDATE ON sync_inbox TO orion_platform;

-- +goose Down
DROP TABLE sync_inbox;
DROP FUNCTION sync_inbox_settled();
