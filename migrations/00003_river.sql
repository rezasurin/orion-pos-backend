-- Tables for river, the Postgres-backed job queue (BACKEND_PLAN.md sections 4.13 and 8).
--
-- This is the end state of river's main migration line up to version 8 (river v0.48), written out
-- as one goose migration so there is a single way to migrate the schema. Generated with
-- `river migrate-get --all --exclude-version 1` and flattened; the two are checked against each
-- other in internal/database. When upgrading river, add a new goose migration with the new
-- river steps instead of editing this one.
--
-- Jobs are inserted by orion_app inside the same transaction as the business write, and run by
-- `orion worker` as orion_platform. Job arguments carry ids only, never secrets or personal data.
-- The queue tables have no tenant_id, so they are listed as unscoped in the schema test.

-- +goose Up
CREATE TYPE river_job_state AS ENUM (
    'available', 'cancelled', 'completed', 'discarded', 'pending', 'retryable', 'running', 'scheduled'
);

CREATE TABLE river_job (
    id            bigserial PRIMARY KEY,
    state         river_job_state NOT NULL DEFAULT 'available',
    attempt       smallint NOT NULL DEFAULT 0,
    max_attempts  smallint NOT NULL DEFAULT 25,
    attempted_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    finalized_at  timestamptz,
    scheduled_at  timestamptz NOT NULL DEFAULT now(),
    priority      smallint NOT NULL DEFAULT 1,
    args          jsonb NOT NULL,
    attempted_by  text[],
    errors        jsonb[],
    kind          text NOT NULL,
    metadata      jsonb NOT NULL DEFAULT '{}',
    queue         text NOT NULL DEFAULT 'default',
    tags          varchar(255)[] NOT NULL DEFAULT '{}',
    unique_key    bytea,
    unique_states bit(8),
    CONSTRAINT finalized_or_finalized_at_null CHECK (
        (finalized_at IS NULL AND state NOT IN ('cancelled', 'completed', 'discarded')) OR
        (finalized_at IS NOT NULL AND state IN ('cancelled', 'completed', 'discarded'))
    ),
    CONSTRAINT max_attempts_is_positive CHECK (max_attempts > 0),
    CONSTRAINT priority_in_range CHECK (priority >= 1 AND priority <= 4),
    CONSTRAINT queue_length CHECK (char_length(queue) > 0 AND char_length(queue) < 128),
    CONSTRAINT kind_length CHECK (char_length(kind) > 0 AND char_length(kind) < 128)
);

CREATE INDEX river_job_kind ON river_job USING btree (kind);
CREATE INDEX river_job_state_and_finalized_at_index ON river_job USING btree (state, finalized_at)
    WHERE finalized_at IS NOT NULL;
CREATE INDEX river_job_prioritized_fetching_index ON river_job USING btree (state, queue, priority, scheduled_at, id);
CREATE INDEX river_job_args_index ON river_job USING gin (args);
CREATE INDEX river_job_metadata_index ON river_job USING gin (metadata);

-- +goose StatementBegin
CREATE FUNCTION river_job_state_in_bitmask(bitmask bit(8), state river_job_state)
    RETURNS boolean
    LANGUAGE sql IMMUTABLE
AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX river_job_unique_idx ON river_job (unique_key)
    WHERE unique_key IS NOT NULL
      AND unique_states IS NOT NULL
      AND river_job_state_in_bitmask(unique_states, state);

CREATE UNLOGGED TABLE river_leader (
    elected_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    leader_id  text NOT NULL,
    name       text PRIMARY KEY DEFAULT 'default',
    CONSTRAINT name_length CHECK (name = 'default'),
    CONSTRAINT leader_id_length CHECK (char_length(leader_id) > 0 AND char_length(leader_id) < 128)
);

CREATE TABLE river_queue (
    name       text PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    paused_at  timestamptz,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE river_notification (
    id         bigserial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    payload    text NOT NULL,
    topic      text NOT NULL,
    CONSTRAINT topic_length CHECK (length(topic) > 0 AND length(topic) < 128)
);

CREATE INDEX river_notification_created_at_idx ON river_notification (created_at);
CREATE INDEX river_notification_topic_id_idx ON river_notification (topic, id);

-- The API inserts jobs and nothing else. RETURNING needs SELECT, and an insert with a uniqueness
-- rule is an upsert that touches `kind` on conflict, hence that one column. The worker, running
-- as orion_platform, owns the rest of the queue.
GRANT SELECT, INSERT ON river_job TO orion_app;
GRANT UPDATE (kind) ON river_job TO orion_app;
GRANT USAGE ON SEQUENCE river_job_id_seq TO orion_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON river_job, river_leader, river_queue, river_notification
    TO orion_platform;
GRANT USAGE ON SEQUENCE river_job_id_seq, river_notification_id_seq TO orion_platform;

-- +goose Down
DROP TABLE river_notification;
DROP TABLE river_queue;
DROP TABLE river_leader;
DROP TABLE river_job;
DROP FUNCTION river_job_state_in_bitmask(bit, river_job_state);
DROP TYPE river_job_state;
