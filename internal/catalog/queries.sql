-- Categories

-- name: InsertCategory :one
INSERT INTO category (id, tenant_id, name, sort_order)
VALUES (@id, @tenant_id, @name, @sort_order)
RETURNING *;

-- name: GetCategoryForUpdate :one
SELECT * FROM category WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListCategories :many
SELECT * FROM category
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateCategory :one
UPDATE category SET name = @name, sort_order = @sort_order, archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- Items

-- name: InsertItem :one
INSERT INTO item (id, tenant_id, category_id, name, sku, barcode, image_url, track_stock)
VALUES (@id, @tenant_id, sqlc.narg(category_id), @name, sqlc.narg(sku), sqlc.narg(barcode), sqlc.narg(image_url), @track_stock)
RETURNING *;

-- name: GetItem :one
SELECT * FROM item WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetItemForUpdate :one
SELECT * FROM item WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListItems :many
SELECT * FROM item
WHERE tenant_id = @tenant_id AND id > @after
  AND (@include_archived::boolean OR archived_at IS NULL)
  AND (sqlc.narg(category_id)::uuid IS NULL OR category_id = sqlc.narg(category_id))
ORDER BY id
LIMIT @page_size;

-- name: UpdateItem :one
UPDATE item
SET category_id = sqlc.narg(category_id), name = @name, sku = sqlc.narg(sku), barcode = sqlc.narg(barcode),
    image_url = sqlc.narg(image_url), track_stock = @track_stock, archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- Variants

-- name: InsertVariant :one
INSERT INTO variant (id, tenant_id, item_id, name, sku, barcode, base_price, sort_order)
VALUES (@id, @tenant_id, @item_id, @name, sqlc.narg(sku), sqlc.narg(barcode), @base_price, @sort_order)
RETURNING *;

-- name: GetVariantForUpdate :one
SELECT * FROM variant WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListVariantsByItems :many
-- The variants of a page of items, in one query.
SELECT * FROM variant
WHERE tenant_id = @tenant_id AND item_id = ANY(@item_ids::uuid[])
ORDER BY item_id, sort_order, id;

-- name: UpdateVariant :one
UPDATE variant
SET name = @name, sku = sqlc.narg(sku), barcode = sqlc.narg(barcode), base_price = @base_price,
    sort_order = @sort_order, archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- Modifier groups and modifiers

-- name: InsertModifierGroup :one
INSERT INTO modifier_group (id, tenant_id, name, min_select, max_select, required)
VALUES (@id, @tenant_id, @name, @min_select, @max_select, @required)
RETURNING *;

-- name: GetModifierGroupForUpdate :one
SELECT * FROM modifier_group WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListModifierGroups :many
SELECT * FROM modifier_group
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateModifierGroup :one
UPDATE modifier_group
SET name = @name, min_select = @min_select, max_select = @max_select, required = @required,
    archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- name: InsertModifier :one
INSERT INTO modifier (id, tenant_id, group_id, name, price_delta, sort_order)
VALUES (@id, @tenant_id, @group_id, @name, @price_delta, @sort_order)
RETURNING *;

-- name: GetModifierForUpdate :one
SELECT * FROM modifier WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListModifiersByGroups :many
-- The modifiers of a page of groups, in one query.
SELECT * FROM modifier
WHERE tenant_id = @tenant_id AND group_id = ANY(@group_ids::uuid[])
ORDER BY group_id, sort_order, id;

-- name: UpdateModifier :one
UPDATE modifier
SET name = @name, price_delta = @price_delta, sort_order = @sort_order, archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- Which modifier groups an item offers

-- name: ListItemModifierGroups :many
-- The group links of a page of items, in one query.
SELECT item_id, group_id FROM item_modifier_group
WHERE tenant_id = @tenant_id AND item_id = ANY(@item_ids::uuid[])
ORDER BY item_id, sort_order, group_id;

-- name: DeleteItemModifierGroups :exec
DELETE FROM item_modifier_group WHERE tenant_id = @tenant_id AND item_id = @item_id;

-- name: InsertItemModifierGroups :exec
-- Display order follows the order of the ids given.
INSERT INTO item_modifier_group (tenant_id, item_id, group_id, sort_order)
SELECT @tenant_id::uuid, @item_id::uuid, g.id, (g.n - 1)::integer
FROM unnest(@group_ids::uuid[]) WITH ORDINALITY AS g(id, n);

-- Per-outlet prices and availability

-- name: GetOutletVariantForUpdate :one
SELECT * FROM outlet_variant WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id AND variant_id = @variant_id FOR UPDATE;

-- name: UpsertOutletVariant :one
INSERT INTO outlet_variant (tenant_id, outlet_id, variant_id, price_override, available)
VALUES (@tenant_id, @outlet_id, @variant_id, sqlc.narg(price_override), @available)
ON CONFLICT (tenant_id, outlet_id, variant_id)
DO UPDATE SET price_override = excluded.price_override, available = excluded.available
RETURNING *;

-- name: ListOutletVariants :many
SELECT * FROM outlet_variant
WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id AND variant_id > @after
ORDER BY variant_id
LIMIT @page_size;

-- Bulk inserts: one statement however many rows (BACKEND_PLAN.md section 4.12). Empty strings in
-- the optional columns become NULL. Position in the arrays is the display order.

-- name: InsertVariants :many
INSERT INTO variant (id, tenant_id, item_id, name, sku, barcode, base_price, sort_order)
SELECT a.id, @tenant_id::uuid, @item_id::uuid, b.name, NULLIF(c.sku, ''), NULLIF(d.barcode, ''), e.base_price, (a.n - 1)::integer
FROM unnest(@ids::uuid[]) WITH ORDINALITY AS a(id, n)
JOIN unnest(@names::text[]) WITH ORDINALITY AS b(name, n) ON b.n = a.n
JOIN unnest(@skus::text[]) WITH ORDINALITY AS c(sku, n) ON c.n = a.n
JOIN unnest(@barcodes::text[]) WITH ORDINALITY AS d(barcode, n) ON d.n = a.n
JOIN unnest(@base_prices::bigint[]) WITH ORDINALITY AS e(base_price, n) ON e.n = a.n
RETURNING *;

-- name: InsertModifiers :many
INSERT INTO modifier (id, tenant_id, group_id, name, price_delta, sort_order)
SELECT a.id, @tenant_id::uuid, @group_id::uuid, b.name, c.price_delta, (a.n - 1)::integer
FROM unnest(@ids::uuid[]) WITH ORDINALITY AS a(id, n)
JOIN unnest(@names::text[]) WITH ORDINALITY AS b(name, n) ON b.n = a.n
JOIN unnest(@price_deltas::bigint[]) WITH ORDINALITY AS c(price_delta, n) ON c.n = a.n
RETURNING *;

-- name: CountVariants :one
SELECT count(*) FROM variant WHERE tenant_id = @tenant_id AND item_id = @item_id;

-- name: ListModifierGroupsByIDs :many
SELECT * FROM modifier_group WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]) ORDER BY id;
