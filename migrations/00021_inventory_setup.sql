-- Inventory setup (BACKEND_PLAN.md 6.6 and 6.6.2, task B3.2): transaction types, ingredient
-- categories, units of measure in categories, and ingredients. Units follow ADR 0009: a unit is an
-- integer fraction of its category's reference unit, and quantities will be stored in thousandths
-- of that reference unit. Nothing here is synced to tablets, so nothing is in the change log.

-- +goose Up
CREATE TABLE transaction_type (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    -- The back office's three groups: raw and supporting materials, services and maintenance,
    -- paying off purchases made on credit.
    category    text NOT NULL CHECK (category IN ('materials', 'services', 'debt_payment')),
    description text CHECK (length(description) <= 500),
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX transaction_type_name_idx ON transaction_type (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE ingredient_category (
    id                          uuid PRIMARY KEY,
    tenant_id                   uuid NOT NULL REFERENCES tenant (id),
    name                        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    default_transaction_type_id uuid, -- what a purchase of its ingredients is booked as, by default
    description                 text CHECK (length(description) <= 500),
    archived_at                 timestamptz,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, default_transaction_type_id) REFERENCES transaction_type (tenant_id, id)
);
CREATE UNIQUE INDEX ingredient_category_name_idx ON ingredient_category (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE uom_category (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX uom_category_name_idx ON uom_category (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE uom (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    category_id     uuid NOT NULL,
    name            text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 30),
    is_reference    boolean NOT NULL DEFAULT false,
    -- One of this unit is ratio_num / ratio_den reference units, stored reduced (ADR 0009).
    ratio_num       bigint NOT NULL CHECK (ratio_num BETWEEN 1 AND 1000000000),
    ratio_den       bigint NOT NULL CHECK (ratio_den BETWEEN 1 AND 1000000000),
    -- Display and entry precision in thousandths of this unit: 10 is 0.01.
    rounding_scaled bigint NOT NULL CHECK (rounding_scaled BETWEEN 1 AND 1000000),
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, category_id) REFERENCES uom_category (tenant_id, id),
    CHECK (gcd(ratio_num, ratio_den) = 1),
    CHECK (NOT is_reference OR (ratio_num = 1 AND ratio_den = 1 AND active))
);
-- At most one reference per category; the service creates it with the category.
CREATE UNIQUE INDEX uom_reference_idx ON uom (category_id) WHERE is_reference;
-- Names are unique within a category; this index also serves loading a category's units.
CREATE UNIQUE INDEX uom_name_idx ON uom (category_id, lower(name));

CREATE TABLE ingredient (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 120),
    category_id uuid,
    -- The unit it is shown and entered in. Its base unit, in which quantities are stored, is the
    -- reference unit of this unit's category.
    uom_id      uuid NOT NULL,
    track       boolean NOT NULL DEFAULT true,
    description text CHECK (length(description) <= 500),
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, category_id) REFERENCES ingredient_category (tenant_id, id),
    FOREIGN KEY (tenant_id, uom_id) REFERENCES uom (tenant_id, id)
);
CREATE UNIQUE INDEX ingredient_name_idx ON ingredient (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TRIGGER transaction_type_set_updated_at BEFORE UPDATE ON transaction_type FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER ingredient_category_set_updated_at BEFORE UPDATE ON ingredient_category FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER uom_category_set_updated_at BEFORE UPDATE ON uom_category FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER uom_set_updated_at BEFORE UPDATE ON uom FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER ingredient_set_updated_at BEFORE UPDATE ON ingredient FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE transaction_type ENABLE ROW LEVEL SECURITY;
ALTER TABLE ingredient_category ENABLE ROW LEVEL SECURITY;
ALTER TABLE uom_category ENABLE ROW LEVEL SECURITY;
ALTER TABLE uom ENABLE ROW LEVEL SECURITY;
ALTER TABLE ingredient ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transaction_type TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON ingredient_category TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON uom_category TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON uom TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON ingredient TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON transaction_type TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON ingredient_category TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON uom_category TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON uom TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON ingredient TO orion_platform USING (true) WITH CHECK (true);

-- Archived or deactivated, never deleted: ledger rows and recipes (B3.3, B3.4) will point at them.
-- A unit's category and whether it is the reference never change.
GRANT SELECT, INSERT ON transaction_type, ingredient_category, uom_category, uom, ingredient TO orion_app, orion_platform;
GRANT UPDATE (name, category, description, archived_at) ON transaction_type TO orion_app;
GRANT UPDATE (name, default_transaction_type_id, description, archived_at) ON ingredient_category TO orion_app;
GRANT UPDATE (name, archived_at) ON uom_category TO orion_app;
GRANT UPDATE (name, ratio_num, ratio_den, rounding_scaled, active) ON uom TO orion_app;
GRANT UPDATE (name, category_id, uom_id, track, description, archived_at) ON ingredient TO orion_app;
GRANT UPDATE ON transaction_type, ingredient_category, uom_category, uom, ingredient TO orion_platform;

-- +goose Down
DROP TABLE ingredient;
DROP TABLE uom;
DROP TABLE uom_category;
DROP TABLE ingredient_category;
DROP TABLE transaction_type;
