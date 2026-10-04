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
