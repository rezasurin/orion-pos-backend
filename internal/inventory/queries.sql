-- Transaction types

-- name: InsertTransactionType :one
INSERT INTO transaction_type (id, tenant_id, name, category, description)
VALUES (@id, @tenant_id, @name, @category, sqlc.narg(description))
RETURNING *;

-- name: GetTransactionTypeForUpdate :one
SELECT * FROM transaction_type WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListTransactionTypes :many
SELECT * FROM transaction_type
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateTransactionType :one
UPDATE transaction_type SET name = @name, category = @category, description = sqlc.narg(description), archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- Ingredient categories

-- name: InsertIngredientCategory :one
INSERT INTO ingredient_category (id, tenant_id, name, default_transaction_type_id, description)
VALUES (@id, @tenant_id, @name, sqlc.narg(default_transaction_type_id), sqlc.narg(description))
RETURNING *;

-- name: GetIngredientCategoryForUpdate :one
SELECT * FROM ingredient_category WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListIngredientCategories :many
SELECT * FROM ingredient_category
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
ORDER BY id
LIMIT @page_size;

-- name: UpdateIngredientCategory :one
UPDATE ingredient_category
SET name = @name, default_transaction_type_id = sqlc.narg(default_transaction_type_id), description = sqlc.narg(description), archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;

-- UoM categories and units

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
INSERT INTO uom (id, tenant_id, category_id, name, is_reference, ratio_num, ratio_den, rounding_scaled, active)
SELECT unnest(@ids::uuid[]), @tenant_id, @category_id, unnest(@names::text[]), unnest(@is_reference::boolean[]),
       unnest(@ratio_num::bigint[]), unnest(@ratio_den::bigint[]), unnest(@rounding_scaled::bigint[]), unnest(@active::boolean[]);

-- name: UpdateUom :execrows
UPDATE uom SET name = @name, ratio_num = @ratio_num, ratio_den = @ratio_den, rounding_scaled = @rounding_scaled, active = @active
WHERE tenant_id = @tenant_id AND category_id = @category_id AND id = @id;

-- name: ListUomsByCategories :many
SELECT * FROM uom WHERE tenant_id = @tenant_id AND category_id = ANY(@category_ids::uuid[]) ORDER BY category_id, NOT is_reference, ratio_num::numeric / ratio_den, id;

-- name: GetUomForIngredient :one
-- A unit an ingredient may be given: active, in a category that is not archived.
SELECT u.id, u.category_id FROM uom u JOIN uom_category c ON c.tenant_id = u.tenant_id AND c.id = u.category_id
WHERE u.tenant_id = @tenant_id AND u.id = @id AND u.active AND c.archived_at IS NULL;

-- Ingredients

-- name: InsertIngredient :one
INSERT INTO ingredient (id, tenant_id, name, category_id, uom_id, track, description)
VALUES (@id, @tenant_id, @name, sqlc.narg(category_id), @uom_id, @track, sqlc.narg(description))
RETURNING *;

-- name: GetIngredientForUpdate :one
SELECT * FROM ingredient WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListIngredients :many
SELECT * FROM ingredient
WHERE tenant_id = @tenant_id AND id > @after AND (@include_archived::boolean OR archived_at IS NULL)
  AND (sqlc.narg(category_id)::uuid IS NULL OR category_id = sqlc.narg(category_id))
ORDER BY id
LIMIT @page_size;

-- name: UpdateIngredient :one
UPDATE ingredient
SET name = @name, category_id = sqlc.narg(category_id), uom_id = @uom_id, track = @track, description = sqlc.narg(description), archived_at = sqlc.narg(archived_at)
WHERE tenant_id = @tenant_id AND id = @id
RETURNING *;
