-- Catalog: categories, items, variants, modifiers and per-outlet prices (BACKEND_PLAN.md section
-- 6.3). Everything a sale may point at is archived, never deleted, so there is no DELETE grant
-- except on the two link tables.

-- +goose Up
CREATE TABLE category (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    sort_order  integer NOT NULL DEFAULT 0,
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);

-- Two live categories cannot share a name (ignoring case); an archived one frees its name.
CREATE UNIQUE INDEX category_name_idx ON category (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE item (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    category_id uuid,
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 120),
    sku         text CHECK (sku IS NULL OR (length(sku) BETWEEN 1 AND 40)),
    barcode     text CHECK (barcode IS NULL OR (length(barcode) BETWEEN 1 AND 40)),
    image_url   text CHECK (image_url IS NULL OR length(image_url) <= 500),
    track_stock boolean NOT NULL DEFAULT false, -- Phase 3: consume ingredients when sold
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    -- A null category_id skips this check (MATCH SIMPLE): items may be uncategorised.
    FOREIGN KEY (tenant_id, category_id) REFERENCES category (tenant_id, id)
);

-- The item list pages by id and filters by category; the primary key covers the paging, and a
-- tenant has hundreds of items at most, so category_id gets no index until measurement asks for it.

CREATE TABLE variant (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    item_id     uuid NOT NULL,
    name        text NOT NULL DEFAULT '' CHECK (length(name) <= 60), -- empty for a one-size item
    sku         text CHECK (sku IS NULL OR (length(sku) BETWEEN 1 AND 40)),
    barcode     text CHECK (barcode IS NULL OR (length(barcode) BETWEEN 1 AND 40)),
    base_price  bigint NOT NULL CHECK (base_price BETWEEN 0 AND 1000000000), -- rupiah
    sort_order  integer NOT NULL DEFAULT 0,
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES item (tenant_id, id)
);

-- Loading the variants of a page of items, and the barcode lookup a scanner does. A live SKU or
-- barcode is unique in a business; archiving a variant frees its code.
CREATE INDEX variant_item_idx ON variant (tenant_id, item_id);
CREATE UNIQUE INDEX variant_sku_idx ON variant (tenant_id, sku) WHERE sku IS NOT NULL AND archived_at IS NULL;
CREATE UNIQUE INDEX variant_barcode_idx ON variant (tenant_id, barcode) WHERE barcode IS NOT NULL AND archived_at IS NULL;

CREATE TABLE modifier_group (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    min_select  integer NOT NULL DEFAULT 0 CHECK (min_select >= 0),
    max_select  integer NOT NULL DEFAULT 1 CHECK (max_select >= 1 AND max_select <= 100),
    required    boolean NOT NULL DEFAULT false,
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    CHECK (min_select <= max_select),
    CHECK (NOT required OR min_select >= 1)
);

CREATE UNIQUE INDEX modifier_group_name_idx ON modifier_group (tenant_id, lower(name)) WHERE archived_at IS NULL;

CREATE TABLE modifier (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    group_id    uuid NOT NULL,
    name        text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    price_delta bigint NOT NULL DEFAULT 0 CHECK (price_delta BETWEEN -1000000000 AND 1000000000),
    sort_order  integer NOT NULL DEFAULT 0,
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, group_id) REFERENCES modifier_group (tenant_id, id)
);

-- Loading the modifiers of a page of groups.
CREATE INDEX modifier_group_idx ON modifier (tenant_id, group_id);

-- Which modifier groups an item offers, in display order. A link, not a business record, so rows
-- are replaced by deleting and inserting; the item's change is what the POS pull sees.
CREATE TABLE item_modifier_group (
    tenant_id  uuid NOT NULL,
    item_id    uuid NOT NULL,
    group_id   uuid NOT NULL,
    sort_order integer NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, item_id, group_id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES item (tenant_id, id),
    FOREIGN KEY (tenant_id, group_id) REFERENCES modifier_group (tenant_id, id)
);

-- A variant at an outlet. No row means the base price and available. The primary key starts with
-- (tenant_id, outlet_id), which is how the outlet's list and the POS pull read it.
CREATE TABLE outlet_variant (
    tenant_id      uuid NOT NULL,
    outlet_id      uuid NOT NULL,
    variant_id     uuid NOT NULL,
    price_override bigint CHECK (price_override IS NULL OR price_override BETWEEN 0 AND 1000000000),
    available      boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, outlet_id, variant_id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, variant_id) REFERENCES variant (tenant_id, id)
);

CREATE TRIGGER category_set_updated_at BEFORE UPDATE ON category
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER item_set_updated_at BEFORE UPDATE ON item
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER variant_set_updated_at BEFORE UPDATE ON variant
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER modifier_group_set_updated_at BEFORE UPDATE ON modifier_group
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER modifier_set_updated_at BEFORE UPDATE ON modifier
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER outlet_variant_set_updated_at BEFORE UPDATE ON outlet_variant
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE category ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON category TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON category TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE item ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON item TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON item TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE variant ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON variant TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON variant TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE modifier_group ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON modifier_group TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON modifier_group TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE modifier ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON modifier TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON modifier TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE item_modifier_group ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON item_modifier_group TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON item_modifier_group TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE outlet_variant ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outlet_variant TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON outlet_variant TO orion_platform USING (true) WITH CHECK (true);

-- Column-level UPDATE grants: ids, owners and parents never change. Nothing is deleted except the
-- item to modifier group links.
GRANT SELECT, INSERT ON category, item, variant, modifier_group, modifier, item_modifier_group, outlet_variant
    TO orion_app, orion_platform;
GRANT UPDATE (name, sort_order, archived_at) ON category TO orion_app;
GRANT UPDATE (category_id, name, sku, barcode, image_url, track_stock, archived_at) ON item TO orion_app;
GRANT UPDATE (name, sku, barcode, base_price, sort_order, archived_at) ON variant TO orion_app;
GRANT UPDATE (name, min_select, max_select, required, archived_at) ON modifier_group TO orion_app;
GRANT UPDATE (name, price_delta, sort_order, archived_at) ON modifier TO orion_app;
GRANT UPDATE (price_override, available) ON outlet_variant TO orion_app;
GRANT DELETE ON item_modifier_group TO orion_app, orion_platform;
GRANT UPDATE ON category, item, variant, modifier_group, modifier, outlet_variant TO orion_platform;

-- +goose Down
DROP TABLE outlet_variant;
DROP TABLE item_modifier_group;
DROP TABLE modifier;
DROP TABLE modifier_group;
DROP TABLE variant;
DROP TABLE item;
DROP TABLE category;
