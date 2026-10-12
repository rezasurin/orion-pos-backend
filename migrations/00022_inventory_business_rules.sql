-- Inventory setup to the back office's business rules (BACKEND_PLAN.md 6.6.3, task B3.2b;
-- Inventory-React docs/BUSINESS_RULES.md). Replaces the B3.2 tables, which held only setup data
-- and were never deployed: transaction types become expense types (BR-EXP-02), ingredients become
-- stock items with a type (BR-ITM-01), units get symbols and a standard set (BR-UOM-02, 03), and an
-- item gets its own packs and a recipe unit (BR-UOM-04, 05).

-- +goose Up
DROP TABLE ingredient;
DROP TABLE uom;
DROP TABLE uom_category;
DROP TABLE ingredient_category;
DROP TABLE transaction_type;

-- Names everywhere: trimmed, 2 to 100 characters, unique per business ignoring case among live
-- rows (BR-GEN-10, validation reference).
CREATE TABLE expense_type (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenant (id),
    name          text NOT NULL CHECK (length(btrim(name)) BETWEEN 2 AND 100 AND name = btrim(name)),
    -- A classification only (BR-EXP-02); whether money is paid or owed is a field of the payment.
    expense_group text NOT NULL CHECK (expense_group IN ('cost_of_goods', 'operating', 'maintenance', 'marketing_event', 'capital')),
    description   text CHECK (length(description) <= 500),
    archived_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX expense_type_name_idx ON expense_type (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE stock_category (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenant (id),
    name                    text NOT NULL CHECK (length(btrim(name)) BETWEEN 2 AND 100 AND name = btrim(name)),
    default_expense_type_id uuid, -- pre-fills a purchase line, never a hard link (BR-CAT-02)
    description             text CHECK (length(description) <= 500),
    archived_at             timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, default_expense_type_id) REFERENCES expense_type (tenant_id, id)
);
CREATE UNIQUE INDEX stock_category_name_idx ON stock_category (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE uom_category (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) BETWEEN 2 AND 100 AND name = btrim(name)),
    is_standard boolean NOT NULL DEFAULT false, -- weight, volume, count: seeded, never archived
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    CHECK (NOT is_standard OR archived_at IS NULL)
);
CREATE UNIQUE INDEX uom_category_name_idx ON uom_category (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE uom (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    category_id     uuid NOT NULL,
    name            text NOT NULL CHECK (length(btrim(name)) BETWEEN 2 AND 60 AND name = btrim(name)),
    symbol          text NOT NULL CHECK (length(btrim(symbol)) BETWEEN 1 AND 10 AND symbol = btrim(symbol)),
    is_reference    boolean NOT NULL DEFAULT false,
    is_standard     boolean NOT NULL DEFAULT false, -- seeded: ratio, symbol and active are fixed
    -- One of this unit is ratio_num / ratio_den reference units, stored reduced (ADR 0009).
    ratio_num       bigint NOT NULL CHECK (ratio_num BETWEEN 1 AND 1000000000),
    ratio_den       bigint NOT NULL CHECK (ratio_den BETWEEN 1 AND 1000000000),
    -- The step a person may enter, in thousandths of this unit: 10 is 0.01 (BR-UOM-06).
    rounding_scaled bigint NOT NULL CHECK (rounding_scaled BETWEEN 1 AND 1000000),
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, category_id) REFERENCES uom_category (tenant_id, id),
    CHECK (gcd(ratio_num, ratio_den) = 1),
    CHECK (NOT is_reference OR (ratio_num = 1 AND ratio_den = 1 AND active)),
    CHECK (NOT is_standard OR active)
);
-- At most one reference per category; the service creates it with the category.
CREATE UNIQUE INDEX uom_reference_idx ON uom (category_id) WHERE is_reference;
-- Names are unique within a category; this index also serves loading a category's units.
CREATE UNIQUE INDEX uom_name_idx ON uom (category_id, lower(name));
-- A symbol names one unit in the whole business (BR-UOM-02), so `kg` is never ambiguous.
CREATE UNIQUE INDEX uom_symbol_idx ON uom (tenant_id, lower(symbol));

CREATE TABLE stock_item (
    id               uuid PRIMARY KEY,
    tenant_id        uuid NOT NULL REFERENCES tenant (id),
    name             text NOT NULL CHECK (length(btrim(name)) BETWEEN 2 AND 100 AND name = btrim(name)),
    type             text NOT NULL CHECK (type IN ('ingredient', 'supporting', 'prepared', 'finished', 'supply')),
    category_id      uuid NOT NULL,
    -- A reference unit: quantities are stored in thousandths of it (ADR 0009). The service checks
    -- that it is a reference and that the recipe unit is in its category.
    base_uom_id      uuid NOT NULL,
    recipe_uom_id    uuid, -- null: recipes use the base unit
    track            boolean NOT NULL DEFAULT true,
    min_stock_scaled bigint CHECK (min_stock_scaled BETWEEN 1 AND 1000000000000000), -- BR-ITM-04, base thousandths
    shelf_life_days  integer CHECK (shelf_life_days BETWEEN 1 AND 3650),                -- BR-ITM-05
    description      text CHECK (length(description) <= 500),
    archived_at      timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, category_id) REFERENCES stock_category (tenant_id, id),
    FOREIGN KEY (tenant_id, base_uom_id) REFERENCES uom (tenant_id, id),
    FOREIGN KEY (tenant_id, recipe_uom_id) REFERENCES uom (tenant_id, id),
    CHECK (track OR min_stock_scaled IS NULL) -- an untracked item has no balance to alert on
);
CREATE UNIQUE INDEX stock_item_name_idx ON stock_item (tenant_id, lower(name)) WHERE archived_at IS NULL;
-- Is a unit used as a recipe unit (BR-UOM-07)? Asked when a unit's ratio would change.
CREATE INDEX stock_item_recipe_uom_idx ON stock_item (recipe_uom_id) WHERE recipe_uom_id IS NOT NULL;

-- Packaging belongs to an item (BR-UOM-04): "Beras: 1 karung = 25 kg" is 25000/1 of its base unit.
CREATE TABLE stock_item_pack (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    stock_item_id   uuid NOT NULL,
    name            text NOT NULL CHECK (length(btrim(name)) BETWEEN 2 AND 60 AND name = btrim(name)),
    ratio_num       bigint NOT NULL CHECK (ratio_num BETWEEN 1 AND 1000000000),
    ratio_den       bigint NOT NULL CHECK (ratio_den BETWEEN 1 AND 1000000000),
    rounding_scaled bigint NOT NULL CHECK (rounding_scaled BETWEEN 1 AND 1000000),
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, stock_item_id) REFERENCES stock_item (tenant_id, id),
    CHECK (gcd(ratio_num, ratio_den) = 1)
);
-- Names are unique per item; this index also serves loading an item's packs.
CREATE UNIQUE INDEX stock_item_pack_name_idx ON stock_item_pack (stock_item_id, lower(name));

CREATE TRIGGER expense_type_set_updated_at BEFORE UPDATE ON expense_type FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER stock_category_set_updated_at BEFORE UPDATE ON stock_category FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER uom_category_set_updated_at BEFORE UPDATE ON uom_category FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER uom_set_updated_at BEFORE UPDATE ON uom FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER stock_item_set_updated_at BEFORE UPDATE ON stock_item FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER stock_item_pack_set_updated_at BEFORE UPDATE ON stock_item_pack FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE expense_type ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_category ENABLE ROW LEVEL SECURITY;
ALTER TABLE uom_category ENABLE ROW LEVEL SECURITY;
ALTER TABLE uom ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_item ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_item_pack ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON expense_type TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON stock_category TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON uom_category TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON uom TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON stock_item TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON stock_item_pack TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON expense_type TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON stock_category TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON uom_category TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON uom TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON stock_item TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON stock_item_pack TO orion_platform USING (true) WITH CHECK (true);

-- Archived or deactivated, never deleted (BR-GEN-06). A unit's category, and whether it is the
-- reference or standard, never change; a pack's ratio never changes (BR-UOM-07: a pack is used by
-- its item from the start).
GRANT SELECT, INSERT ON expense_type, stock_category, uom_category, uom, stock_item, stock_item_pack TO orion_app, orion_platform;
GRANT UPDATE (name, expense_group, description, archived_at) ON expense_type TO orion_app;
GRANT UPDATE (name, default_expense_type_id, description, archived_at) ON stock_category TO orion_app;
GRANT UPDATE (name, archived_at) ON uom_category TO orion_app;
GRANT UPDATE (name, symbol, ratio_num, ratio_den, rounding_scaled, active) ON uom TO orion_app;
GRANT UPDATE (name, type, category_id, base_uom_id, recipe_uom_id, track, min_stock_scaled, shelf_life_days, description, archived_at) ON stock_item TO orion_app;
GRANT UPDATE (name, rounding_scaled, active) ON stock_item_pack TO orion_app;
GRANT UPDATE ON expense_type, stock_category, uom_category, uom, stock_item, stock_item_pack TO orion_platform;

-- The standard units every business starts with (BR-UOM-03). Steps follow BR-UOM-06: 0.01 for kg
-- and L, whole units for g, ml and pieces. Called by tenancy when a business is created, inside its
-- transaction, and below for the businesses that exist already.
-- +goose StatementBegin
CREATE FUNCTION seed_standard_units(t uuid) RETURNS void LANGUAGE sql AS $$
    WITH c AS (
        INSERT INTO uom_category (id, tenant_id, name, is_standard)
        VALUES (gen_random_uuid(), t, 'Berat', true), (gen_random_uuid(), t, 'Volume', true), (gen_random_uuid(), t, 'Jumlah', true)
        RETURNING id, name
    )
    INSERT INTO uom (id, tenant_id, category_id, name, symbol, is_reference, is_standard, ratio_num, ratio_den, rounding_scaled)
    SELECT gen_random_uuid(), t, c.id, u.name, u.symbol, u.is_ref, true, u.num, 1, u.step
    FROM c JOIN (VALUES
        ('Berat', 'gram', 'g', true, 1, 1000), ('Berat', 'kilogram', 'kg', false, 1000, 10),
        ('Volume', 'mililiter', 'ml', true, 1, 1000), ('Volume', 'liter', 'L', false, 1000, 10),
        ('Jumlah', 'pcs', 'pcs', true, 1, 1000), ('Jumlah', 'lusin', 'lusin', false, 12, 1000), ('Jumlah', 'kodi', 'kodi', false, 20, 1000)
    ) AS u (category, name, symbol, is_ref, num, step) ON u.category = c.name;
$$;
-- +goose StatementEnd

SELECT seed_standard_units(id) FROM tenant;

-- +goose Down
DROP FUNCTION seed_standard_units(uuid);
DROP TABLE stock_item_pack;
DROP TABLE stock_item;
DROP TABLE uom;
DROP TABLE uom_category;
DROP TABLE stock_category;
DROP TABLE expense_type;

-- Back to the B3.2 tables (00021), empty.
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

