-- Roles, permissions, staff and the tenant audit log (BACKEND_PLAN.md sections 4.4, 4.7 and 6.1).

-- +goose Up
CREATE TABLE role (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL REFERENCES tenant (id),
    name       text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 40),
    is_system  boolean NOT NULL DEFAULT false, -- Owner, Manager, Cashier, Kitchen: seeded, not editable
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name)
);

-- Permissions are fixed strings in code (internal/identity/permissions.go); the database only
-- checks their shape.
CREATE TABLE role_permission (
    tenant_id  uuid NOT NULL,
    role_id    uuid NOT NULL,
    permission text NOT NULL CHECK (permission ~ '^[a-z_]+(\.[a-z_]+)+$'),
    PRIMARY KEY (tenant_id, role_id, permission),
    FOREIGN KEY (tenant_id, role_id) REFERENCES role (tenant_id, id)
);

-- A person who acts in a tenant: a cashier with a PIN, or an owner or manager who also signs in
-- by email (user_id set).
CREATE TABLE staff (
    id             uuid PRIMARY KEY,
    tenant_id      uuid NOT NULL REFERENCES tenant (id),
    user_id        uuid,
    display_name   text NOT NULL CHECK (length(btrim(display_name)) > 0 AND length(display_name) <= 60),
    -- argon2id in PHC format. It is shipped to paired devices, which check the PIN offline.
    pin_hash       text,
    pin_rotated_at timestamptz,
    active         boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    CHECK ((pin_hash IS NULL) = (pin_rotated_at IS NULL)),
    -- A staff row for a user means that user is a member of the tenant. Removing the membership
    -- keeps the staff row, since sales and audit entries point at it, and only unlinks the login.
    FOREIGN KEY (tenant_id, user_id) REFERENCES tenant_member (tenant_id, user_id) ON DELETE SET NULL (user_id)
);

-- One staff row per signed-in member; also the access lookup on every request.
CREATE UNIQUE INDEX staff_user_idx ON staff (tenant_id, user_id) WHERE user_id IS NOT NULL;

-- Which roles a person has at which outlet. The primary key starts with tenant_id, and a tenant
-- has tens of staff, so reading one outlet's assignments scans a few hundred rows at most; no
-- separate index on outlet_id until measurement says otherwise.
CREATE TABLE staff_outlet_role (
    tenant_id  uuid NOT NULL,
    staff_id   uuid NOT NULL,
    outlet_id  uuid NOT NULL,
    role_id    uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, staff_id, outlet_id, role_id),
    FOREIGN KEY (tenant_id, staff_id) REFERENCES staff (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, role_id) REFERENCES role (tenant_id, id)
);

-- Sensitive actions inside a tenant: PIN rotation, role and staff changes, device pairing and
-- revocation. Append-only, enforced twice: no UPDATE or DELETE grant, and a trigger. The detail
-- never holds secrets. No index yet: nothing reads this table until the audit view is built, and
-- that query (tenant_id, id DESC) will bring its index.
CREATE TABLE tenant_audit_log (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    actor_type  text NOT NULL CHECK (actor_type IN ('user', 'device', 'system')),
    actor_id    uuid,
    action      text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    target_type text NOT NULL,
    target_id   uuid,
    detail      jsonb NOT NULL DEFAULT '{}',
    ip          inet,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER role_set_updated_at BEFORE UPDATE ON role
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER staff_set_updated_at BEFORE UPDATE ON staff
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER tenant_audit_log_append_only BEFORE UPDATE OR DELETE ON tenant_audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE role ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON role TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON role TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE role_permission ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON role_permission TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON role_permission TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE staff ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON staff TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON staff TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE staff_outlet_role ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON staff_outlet_role TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON staff_outlet_role TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE tenant_audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_audit_log TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON tenant_audit_log TO orion_platform USING (true) WITH CHECK (true);

-- Roles are seeded and read; editing custom roles arrives with its endpoints. Assignments are
-- the one place rows are deleted, since a role assignment is a link, not a business record: the
-- change goes to the audit log and the change log.
GRANT SELECT, INSERT ON role, role_permission, tenant_audit_log TO orion_app, orion_platform;
GRANT SELECT, INSERT ON staff TO orion_app;
GRANT UPDATE (display_name, pin_hash, pin_rotated_at, active) ON staff TO orion_app;
GRANT SELECT, INSERT, UPDATE ON staff TO orion_platform;
GRANT SELECT, INSERT, DELETE ON staff_outlet_role TO orion_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON staff_outlet_role TO orion_platform;

-- +goose Down
DROP TABLE tenant_audit_log;
DROP TABLE staff_outlet_role;
DROP TABLE staff;
DROP TABLE role_permission;
DROP TABLE role;
