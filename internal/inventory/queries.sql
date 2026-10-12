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

-- name: ItemHasMovements :one
SELECT EXISTS (SELECT 1 FROM stock_balance WHERE tenant_id = @tenant_id AND stock_item_id = @stock_item_id AND seq > 0);

-- name: ItemHasStock :one
SELECT EXISTS (SELECT 1 FROM stock_balance WHERE tenant_id = @tenant_id AND stock_item_id = @stock_item_id AND quantity_scaled <> 0);

-- The stock ledger

-- name: ItemsForPosting :many
SELECT id, track, archived_at FROM stock_item WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]);

-- name: MovementsToReverse :many
SELECT * FROM stock_movement WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]);

-- name: LockBalance :one
-- Creates the balance on the first movement, and locks it either way: movements of one item at one
-- outlet are posted one after another.
INSERT INTO stock_balance (tenant_id, outlet_id, stock_item_id, quantity_scaled, value, seq)
VALUES (@tenant_id, @outlet_id, @stock_item_id, 0, 0, 0)
ON CONFLICT (tenant_id, outlet_id, stock_item_id) DO UPDATE SET seq = stock_balance.seq
RETURNING *;

-- name: InsertMovement :one
INSERT INTO stock_movement (id, tenant_id, outlet_id, stock_item_id, seq, kind, quantity_scaled, value, reason, note, staff_id,
                            source_type, source_id, reverses_id, occurred_at, business_date)
VALUES (@id, @tenant_id, @outlet_id, @stock_item_id, @seq, @kind, @quantity_scaled, @value, sqlc.narg(reason), sqlc.narg(note), @staff_id,
        sqlc.narg(source_type), sqlc.narg(source_id), sqlc.narg(reverses_id), @occurred_at, @business_date)
RETURNING *;

-- name: UpdateBalance :exec
UPDATE stock_balance
SET quantity_scaled = @quantity_scaled, value = @value, cost_quantity = sqlc.narg(cost_quantity), cost_value = sqlc.narg(cost_value), seq = @seq, updated_at = now()
WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id AND stock_item_id = @stock_item_id;

-- name: BalanceDrift :many
-- Balances that differ from the ledger, for one business or (with a null tenant, as the platform
-- role) all of them. The ledger's totals are its sums in posting order; the last known average is
-- the running totals at the last movement that left the quantity above 0.
WITH running AS (
    SELECT tenant_id, outlet_id, stock_item_id, seq,
           sum(quantity_scaled) OVER w AS run_quantity, sum(value) OVER w AS run_value
    FROM stock_movement
    WHERE sqlc.narg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.narg(tenant_id)
    WINDOW w AS (PARTITION BY tenant_id, outlet_id, stock_item_id ORDER BY seq)
), ledger AS (
    SELECT tenant_id, outlet_id, stock_item_id, count(*)::bigint AS movements, max(seq)::bigint AS seq,
           (array_agg(run_quantity ORDER BY seq DESC))[1]::bigint AS quantity_scaled,
           (array_agg(run_value ORDER BY seq DESC))[1]::bigint AS value,
           ((array_agg(run_quantity ORDER BY seq DESC) FILTER (WHERE run_quantity > 0))[1])::bigint AS cost_quantity,
           ((array_agg(run_value ORDER BY seq DESC) FILTER (WHERE run_quantity > 0))[1])::bigint AS cost_value
    FROM running GROUP BY tenant_id, outlet_id, stock_item_id
), stored AS (
    SELECT * FROM stock_balance WHERE sqlc.narg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.narg(tenant_id)
)
SELECT coalesce(l.tenant_id, b.tenant_id)::uuid AS tenant_id, coalesce(l.outlet_id, b.outlet_id)::uuid AS outlet_id,
       coalesce(l.stock_item_id, b.stock_item_id)::uuid AS stock_item_id,
       coalesce(l.movements, 0)::bigint AS movements, coalesce(l.seq, 0)::bigint AS ledger_seq,
       coalesce(l.quantity_scaled, 0)::bigint AS ledger_quantity, coalesce(l.value, 0)::bigint AS ledger_value,
       l.cost_quantity AS ledger_cost_quantity, l.cost_value AS ledger_cost_value,
       b.seq AS stored_seq, b.quantity_scaled AS stored_quantity, b.value AS stored_value
FROM ledger l FULL JOIN stored b USING (tenant_id, outlet_id, stock_item_id)
WHERE b.tenant_id IS NULL
   OR l.movements IS DISTINCT FROM l.seq
   OR coalesce(l.seq, 0) <> b.seq OR coalesce(l.quantity_scaled, 0) <> b.quantity_scaled OR coalesce(l.value, 0) <> b.value
   OR l.cost_quantity IS DISTINCT FROM b.cost_quantity OR l.cost_value IS DISTINCT FROM b.cost_value
ORDER BY 1, 2, 3;

-- name: RepairBalance :exec
-- Sets a balance to what the ledger says (BR-STK-08). The ledger itself is never changed.
INSERT INTO stock_balance (tenant_id, outlet_id, stock_item_id, quantity_scaled, value, cost_quantity, cost_value, seq)
VALUES (@tenant_id, @outlet_id, @stock_item_id, @quantity_scaled, @value, sqlc.narg(cost_quantity), sqlc.narg(cost_value), @seq)
ON CONFLICT (tenant_id, outlet_id, stock_item_id) DO UPDATE
SET quantity_scaled = excluded.quantity_scaled, value = excluded.value, cost_quantity = excluded.cost_quantity,
    cost_value = excluded.cost_value, seq = excluded.seq, updated_at = now();
