-- Expense types

-- name: InsertExpenseType :one
INSERT INTO expense_type (id, tenant_id, name, expense_group, description)
VALUES (@id, @tenant_id, @name, @expense_group, sqlc.narg(description))
RETURNING *;

-- name: GetExpenseTypeForUpdate :one
SELECT * FROM expense_type WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListExpenseTypes :many
SELECT * FROM expense_type
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateExpenseType :one
UPDATE expense_type SET name = @name, expense_group = @expense_group, description = sqlc.narg(description), archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- name: IsLiveExpenseType :one
SELECT EXISTS (SELECT 1 FROM expense_type WHERE tenant_id = @tenant_id AND id = @id AND archived_at IS NULL);

-- Stock categories

-- name: InsertStockCategory :one
INSERT INTO stock_category (id, tenant_id, name, default_expense_type_id, description)
VALUES (@id, @tenant_id, @name, sqlc.narg(default_expense_type_id), sqlc.narg(description))
RETURNING *;

-- name: GetStockCategoryForUpdate :one
SELECT * FROM stock_category WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListStockCategories :many
SELECT * FROM stock_category
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateStockCategory :one
UPDATE stock_category
SET name = @name, default_expense_type_id = sqlc.narg(default_expense_type_id), description = sqlc.narg(description), archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- name: IsLiveStockCategory :one
SELECT EXISTS (SELECT 1 FROM stock_category WHERE tenant_id = @tenant_id AND id = @id AND archived_at IS NULL);

-- UoM categories and units

-- name: SeedStandardUnits :exec
SELECT seed_standard_units(@tenant_id::uuid);

-- name: InsertUomCategory :one
INSERT INTO uom_category (id, tenant_id, name)
VALUES (@id, @tenant_id, @name)
RETURNING *;

-- name: GetUomCategoryForUpdate :one
SELECT * FROM uom_category WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListUomCategories :many
SELECT * FROM uom_category
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateUomCategory :one
UPDATE uom_category SET name = @name, archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- name: InsertUoms :exec
INSERT INTO uom (id, tenant_id, category_id, name, symbol, is_reference, ratio_num, ratio_den, rounding_scaled, active)
SELECT unnest(@ids::uuid[]), @tenant_id, @category_id, unnest(@names::text[]), unnest(@symbols::text[]), unnest(@is_reference::boolean[]),
       unnest(@ratio_num::bigint[]), unnest(@ratio_den::bigint[]), unnest(@rounding_scaled::bigint[]), unnest(@active::boolean[]);

-- name: UpdateUom :execrows
UPDATE uom SET name = @name, symbol = @symbol, ratio_num = @ratio_num, ratio_den = @ratio_den, rounding_scaled = @rounding_scaled, active = @active
WHERE tenant_id = @tenant_id AND category_id = @category_id AND id = @id;

-- name: ListUomsByCategories :many
SELECT * FROM uom WHERE tenant_id = @tenant_id AND category_id = ANY(@category_ids::uuid[]) ORDER BY category_id, NOT is_reference, ratio_num::numeric / ratio_den, id;

-- name: UnitsUsedByItems :many
-- Which of these units an item uses as its base or recipe unit (BR-UOM-07).
SELECT DISTINCT u.id FROM uom u
WHERE u.tenant_id = @tenant_id AND u.id = ANY(@ids::uuid[])
  AND EXISTS (SELECT 1 FROM stock_item i WHERE i.tenant_id = u.tenant_id AND (i.base_uom_id = u.id OR i.recipe_uom_id = u.id));

-- name: GetUnitForItem :one
-- A unit an item may be given: its row, and whether it is active in a category that is not archived.
SELECT u.id, u.category_id, u.is_reference, u.rounding_scaled, (u.active AND c.archived_at IS NULL)::boolean AS usable
FROM uom u JOIN uom_category c ON c.tenant_id = u.tenant_id AND c.id = u.category_id
WHERE u.tenant_id = @tenant_id AND u.id = @id;

-- Stock items and their packs

-- name: InsertStockItem :one
INSERT INTO stock_item (id, tenant_id, name, type, category_id, base_uom_id, recipe_uom_id, track, min_stock_scaled, shelf_life_days, description)
VALUES (@id, @tenant_id, @name, @type, @category_id, @base_uom_id, sqlc.narg(recipe_uom_id), @track, sqlc.narg(min_stock_scaled), sqlc.narg(shelf_life_days), sqlc.narg(description))
RETURNING *;

-- name: GetStockItemForUpdate :one
SELECT * FROM stock_item WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListStockItems :many
SELECT * FROM stock_item
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
  AND (sqlc.narg(category_id)::uuid IS NULL OR category_id = sqlc.narg(category_id))
  AND (sqlc.narg(type)::text IS NULL OR type = sqlc.narg(type))
ORDER BY id
LIMIT @page_size;

-- name: UpdateStockItem :one
UPDATE stock_item
SET name = @name, type = @type, category_id = @category_id, base_uom_id = @base_uom_id, recipe_uom_id = sqlc.narg(recipe_uom_id), track = @track,
    min_stock_scaled = sqlc.narg(min_stock_scaled), shelf_life_days = sqlc.narg(shelf_life_days), description = sqlc.narg(description), archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- name: InsertPacks :exec
INSERT INTO stock_item_pack (id, tenant_id, stock_item_id, name, ratio_num, ratio_den, rounding_scaled, active)
SELECT unnest(@ids::uuid[]), @tenant_id, @stock_item_id, unnest(@names::text[]),
       unnest(@ratio_num::bigint[]), unnest(@ratio_den::bigint[]), unnest(@rounding_scaled::bigint[]), unnest(@active::boolean[]);

-- name: UpdatePack :execrows
UPDATE stock_item_pack SET name = @name, rounding_scaled = @rounding_scaled, active = @active
WHERE tenant_id = @tenant_id AND stock_item_id = @stock_item_id AND id = @id;

-- name: ListPacksByItems :many
SELECT * FROM stock_item_pack WHERE tenant_id = @tenant_id AND stock_item_id = ANY(@item_ids::uuid[]) ORDER BY stock_item_id, ratio_num::numeric / ratio_den, id;
