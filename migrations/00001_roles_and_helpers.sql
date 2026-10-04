-- Group roles, shared helper functions and triggers.
--
-- orion_app      is used by every request on tenant routes. Row-level security confines it to
--                the tenant named in the transaction-local setting app.tenant_id.
-- orion_platform is used only by the platform module and named cross-tenant jobs. Policies
--                written for it allow every row.
--
-- Both are NOLOGIN group roles. Each environment creates its own LOGIN roles and grants them
-- membership (see deploy/README.md). Roles are cluster-wide, so they are created only if missing
-- and are not dropped on the way down.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'orion_app') THEN
        CREATE ROLE orion_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'orion_platform') THEN
        CREATE ROLE orion_platform NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO orion_app, orion_platform;
GRANT SELECT ON goose_db_version TO orion_app, orion_platform;

-- The tenant of the current transaction, or NULL when none is set. A NULL tenant matches no row,
-- so a query run without a tenant context fails closed.
-- +goose StatementBegin
CREATE FUNCTION current_tenant_id() RETURNS uuid
    LANGUAGE sql STABLE
AS $$
    SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Backstop for append-only tables, on top of the missing UPDATE and DELETE grants.
-- +goose StatementBegin
CREATE FUNCTION forbid_mutation() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only; % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION forbid_mutation();
DROP FUNCTION set_updated_at();
DROP FUNCTION current_tenant_id();
REVOKE SELECT ON goose_db_version FROM orion_app, orion_platform;
REVOKE USAGE ON SCHEMA public FROM orion_app, orion_platform;
