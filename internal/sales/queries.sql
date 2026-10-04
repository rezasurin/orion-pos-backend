-- Shifts

-- name: InsertShift :exec
INSERT INTO shift (id, tenant_id, outlet_id, device_id, opened_by, opened_at, opening_cash, business_date, received_at)
VALUES (@id, @tenant_id, @outlet_id, @device_id, @opened_by, @opened_at, @opening_cash, @business_date, @received_at);

-- name: GetShiftForUpdate :one
SELECT * FROM shift WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetShift :one
SELECT * FROM shift WHERE tenant_id = @tenant_id AND id = @id;

-- name: CloseShift :execrows
UPDATE shift SET closed_by = @closed_by, closed_at = @closed_at, counted_cash = @counted_cash, close_event_id = @close_event_id
WHERE tenant_id = @tenant_id AND id = @id AND closed_at IS NULL;

-- name: InsertCashMovement :exec
INSERT INTO cash_movement (id, tenant_id, outlet_id, shift_id, kind, amount, reason, staff_id, device_time, received_at, business_date)
VALUES (@id, @tenant_id, @outlet_id, @shift_id, @kind, @amount, @reason, @staff_id, @device_time, @received_at, @business_date);

-- Sales

-- name: InsertSale :exec
INSERT INTO sale (id, tenant_id, outlet_id, device_id, shift_id, staff_id, receipt_number, receipt_device_code, receipt_counter,
                  device_time, received_at, business_date, pricing_version, pricing, catalog_seq, subtotal, discount_total,
                  service_charge, tax, tax_included, rounding_amount, total)
VALUES (@id, @tenant_id, @outlet_id, @device_id, @shift_id, @staff_id, @receipt_number, @receipt_device_code, @receipt_counter,
        @device_time, @received_at, @business_date, @pricing_version, @pricing, @catalog_seq, @subtotal, @discount_total,
        @service_charge, @tax, @tax_included, @rounding_amount, @total);

-- name: InsertSaleLine :batchexec
INSERT INTO sale_line (id, tenant_id, sale_id, line_no, variant_id, name_snapshot, unit_price, quantity, line_discount,
                       allocated_bill_discount, line_total)
VALUES (@id, @tenant_id, @sale_id, @line_no, @variant_id, @name_snapshot, @unit_price, @quantity, @line_discount,
        @allocated_bill_discount, @line_total);

-- name: InsertSaleLineModifier :batchexec
INSERT INTO sale_line_modifier (tenant_id, sale_line_id, position, modifier_id, name_snapshot, price_delta)
VALUES (@tenant_id, @sale_line_id, @position, @modifier_id, @name_snapshot, @price_delta);

-- name: InsertSaleDiscount :batchexec
INSERT INTO sale_discount (id, tenant_id, sale_id, sale_line_id, kind, value, amount, reason, approved_by)
VALUES (@id, @tenant_id, @sale_id, sqlc.narg(sale_line_id), @kind, @value, @amount, @reason, sqlc.narg(approved_by));

-- name: InsertPayment :batchexec
INSERT INTO payment (id, tenant_id, sale_id, method, amount, tendered, change, reference)
VALUES (@id, @tenant_id, @sale_id, @method, @amount, sqlc.narg(tendered), sqlc.narg(change), @reference);

-- name: InsertFlag :batchexec
INSERT INTO flag (id, tenant_id, outlet_id, device_id, event_id, target_type, target_id, code, detail)
VALUES (@id, @tenant_id, @outlet_id, @device_id, @event_id, @target_type, @target_id, @code, @detail);

-- Voids

-- name: GetSaleForVoid :one
SELECT id, outlet_id, shift_id, status FROM sale WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: MarkSaleVoided :exec
UPDATE sale SET status = 'voided' WHERE tenant_id = @tenant_id AND id = @id AND status = 'completed';

-- name: InsertVoid :exec
INSERT INTO void (id, tenant_id, outlet_id, sale_id, shift_id, staff_id, approved_by, reason, device_time, received_at, business_date)
VALUES (@id, @tenant_id, @outlet_id, @sale_id, @shift_id, @staff_id, sqlc.narg(approved_by), @reason, @device_time, @received_at, @business_date);

-- The back-office sales list and detail (read-only).

-- name: ListSales :many
-- Newest business day first, then newest sale by the device's clock, keyset-paged by
-- (business_date, device_time, id). outlet_ids NULL means every outlet.
SELECT s.id, s.outlet_id, s.device_id, s.shift_id, s.staff_id, s.receipt_number, s.device_time, s.received_at, s.business_date,
       s.status, s.subtotal, s.discount_total, s.service_charge, s.tax, s.tax_included, s.rounding_amount, s.total
FROM sale s
WHERE s.tenant_id = @tenant_id
  AND (sqlc.narg(outlet_ids)::uuid[] IS NULL OR s.outlet_id = ANY(sqlc.narg(outlet_ids)::uuid[]))
  AND (sqlc.narg(from_date)::date IS NULL OR s.business_date >= sqlc.narg(from_date)::date)
  AND (sqlc.narg(to_date)::date IS NULL OR s.business_date <= sqlc.narg(to_date)::date)
  AND (sqlc.narg(status)::text IS NULL OR s.status = sqlc.narg(status)::text)
  AND (sqlc.narg(receipt_number)::text IS NULL OR s.receipt_number = sqlc.narg(receipt_number)::text)
  AND (sqlc.narg(staff_id)::uuid IS NULL OR s.staff_id = sqlc.narg(staff_id)::uuid)
  AND (NOT @flagged_only::boolean OR EXISTS (
        SELECT 1 FROM flag f WHERE f.tenant_id = s.tenant_id AND f.target_id = s.id
        UNION ALL
        SELECT 1 FROM flag f JOIN void v ON v.tenant_id = f.tenant_id AND v.id = f.target_id
        WHERE f.tenant_id = s.tenant_id AND v.sale_id = s.id))
  AND (sqlc.narg(after_date)::date IS NULL OR (s.business_date, s.device_time, s.id) < (sqlc.narg(after_date)::date, sqlc.narg(after_time)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY s.business_date DESC, s.device_time DESC, s.id DESC
LIMIT @page_size;

-- name: ListPaymentsBySales :many
SELECT id, sale_id, method, amount, tendered, change, reference, status FROM payment
WHERE tenant_id = @tenant_id AND sale_id = ANY(@sale_ids::uuid[])
ORDER BY sale_id, id;

-- name: ListFlagsBySales :many
-- The flags about each sale and about its void, in one query.
SELECT f.id, f.code, f.target_type, f.target_id, f.detail, f.created_at, coalesce(v.sale_id, f.target_id)::uuid AS sale_id
FROM flag f
LEFT JOIN void v ON v.tenant_id = f.tenant_id AND v.id = f.target_id AND f.target_type = 'void'
WHERE f.tenant_id = @tenant_id AND (f.target_id = ANY(@sale_ids::uuid[]) OR v.sale_id = ANY(@sale_ids::uuid[]))
ORDER BY f.created_at, f.id;

-- name: GetSale :one
SELECT * FROM sale WHERE tenant_id = @tenant_id AND id = @id;

-- name: ListSaleLines :many
SELECT * FROM sale_line WHERE tenant_id = @tenant_id AND sale_id = @sale_id ORDER BY line_no;

-- name: ListSaleLineModifiers :many
SELECT m.* FROM sale_line_modifier m
JOIN sale_line l ON l.tenant_id = m.tenant_id AND l.id = m.sale_line_id
WHERE l.tenant_id = @tenant_id AND l.sale_id = @sale_id
ORDER BY l.line_no, m.position;

-- name: ListSaleDiscounts :many
SELECT d.id, d.sale_line_id, l.line_no AS line_no, d.kind, d.value, d.amount, d.reason, d.approved_by
FROM sale_discount d
LEFT JOIN sale_line l ON l.tenant_id = d.tenant_id AND l.id = d.sale_line_id
WHERE d.tenant_id = @tenant_id AND d.sale_id = @sale_id
ORDER BY d.id;

-- name: GetVoidOfSale :one
SELECT * FROM void WHERE tenant_id = @tenant_id AND sale_id = @sale_id;
