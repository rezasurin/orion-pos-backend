-- Reports read the sales tables and never write them. The columns they use are checked against the
-- migrations when sqlc generates this code, so a schema change in sales that breaks a report fails
-- the build.

-- name: GetShiftForReport :one
SELECT id, outlet_id, device_id, opened_by, opened_at, opening_cash, business_date, closed_by, closed_at, counted_cash
FROM shift WHERE tenant_id = @tenant_id AND id = @id;

-- name: ShiftsOfDay :many
SELECT id, outlet_id, device_id, opened_by, opened_at, opening_cash, business_date, closed_by, closed_at, counted_cash
FROM shift
WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id AND business_date = @business_date
ORDER BY opened_at, id;

-- name: ShiftCash :many
-- The cash that moved through each shift's drawer, for many shifts at once.
--   received: cash applied to the bills of sales rung up in the shift (net of change given), whatever
--             became of the sale later;
--   refunded: cash returned for sales voided in this shift, whichever shift sold them;
--   pay in, pay out, and how many times the drawer was opened without a sale.
SELECT s.id AS shift_id,
    coalesce((SELECT sum(p.amount) FROM sale sa JOIN payment p ON p.tenant_id = sa.tenant_id AND p.sale_id = sa.id
              WHERE sa.tenant_id = s.tenant_id AND sa.shift_id = s.id AND p.method = 'cash' AND p.status = 'confirmed'), 0)::bigint AS received,
    coalesce((SELECT sum(p.amount) FROM void v JOIN payment p ON p.tenant_id = v.tenant_id AND p.sale_id = v.sale_id
              WHERE v.tenant_id = s.tenant_id AND v.shift_id = s.id AND p.method = 'cash' AND p.status = 'confirmed'), 0)::bigint AS refunded,
    coalesce((SELECT sum(m.amount) FROM cash_movement m WHERE m.tenant_id = s.tenant_id AND m.shift_id = s.id AND m.kind = 'pay_in'), 0)::bigint AS pay_in,
    coalesce((SELECT sum(m.amount) FROM cash_movement m WHERE m.tenant_id = s.tenant_id AND m.shift_id = s.id AND m.kind = 'pay_out'), 0)::bigint AS pay_out,
    (SELECT count(*) FROM cash_movement m WHERE m.tenant_id = s.tenant_id AND m.shift_id = s.id AND m.kind = 'no_sale')::bigint AS no_sales
FROM shift s
WHERE s.tenant_id = @tenant_id AND s.id = ANY(@shift_ids::uuid[]);

-- name: ShiftSales :one
-- Completed sales of one shift, and the sales of it that were voided.
SELECT
    count(*) FILTER (WHERE status = 'completed')::bigint AS sales,
    coalesce(sum(subtotal) FILTER (WHERE status = 'completed'), 0)::bigint AS subtotal,
    coalesce(sum(discount_total) FILTER (WHERE status = 'completed'), 0)::bigint AS discount_total,
    coalesce(sum(service_charge) FILTER (WHERE status = 'completed'), 0)::bigint AS service_charge,
    coalesce(sum(tax) FILTER (WHERE status = 'completed'), 0)::bigint AS tax,
    coalesce(sum(rounding_amount) FILTER (WHERE status = 'completed'), 0)::bigint AS rounding,
    coalesce(sum(total) FILTER (WHERE status = 'completed'), 0)::bigint AS total,
    count(*) FILTER (WHERE status = 'voided')::bigint AS voided,
    coalesce(sum(total) FILTER (WHERE status = 'voided'), 0)::bigint AS voided_total
FROM sale WHERE tenant_id = @tenant_id AND shift_id = @shift_id;

-- name: ShiftPaymentMethods :many
SELECT p.method, count(*)::bigint AS payments, sum(p.amount)::bigint AS amount
FROM sale sa JOIN payment p ON p.tenant_id = sa.tenant_id AND p.sale_id = sa.id
WHERE sa.tenant_id = @tenant_id AND sa.shift_id = @shift_id AND sa.status = 'completed' AND p.status = 'confirmed'
GROUP BY p.method ORDER BY p.method;

-- name: ShiftDiscounts :one
SELECT count(*)::bigint AS discounts, coalesce(sum(d.amount), 0)::bigint AS amount
FROM sale sa JOIN sale_discount d ON d.tenant_id = sa.tenant_id AND d.sale_id = sa.id
WHERE sa.tenant_id = @tenant_id AND sa.shift_id = @shift_id AND sa.status = 'completed';

-- name: ShiftVoidsRecorded :one
-- Voids made during the shift, and what they refunded, whichever shift sold the sale.
SELECT count(*)::bigint AS voids, coalesce(sum(sa.total), 0)::bigint AS total
FROM void v JOIN sale sa ON sa.tenant_id = v.tenant_id AND sa.id = v.sale_id
WHERE v.tenant_id = @tenant_id AND v.shift_id = @shift_id;

-- name: ShiftFlagCount :one
SELECT count(*)::bigint FROM flag f
WHERE f.tenant_id = @tenant_id AND (f.target_id = @shift_id OR f.target_id IN (SELECT sa.id FROM sale sa WHERE sa.tenant_id = @tenant_id AND sa.shift_id = @shift_id));

-- name: DaySales :one
SELECT
    count(*) FILTER (WHERE status = 'completed')::bigint AS sales,
    coalesce(sum(subtotal) FILTER (WHERE status = 'completed'), 0)::bigint AS subtotal,
    coalesce(sum(discount_total) FILTER (WHERE status = 'completed'), 0)::bigint AS discount_total,
    coalesce(sum(service_charge) FILTER (WHERE status = 'completed'), 0)::bigint AS service_charge,
    coalesce(sum(tax) FILTER (WHERE status = 'completed'), 0)::bigint AS tax,
    coalesce(sum(rounding_amount) FILTER (WHERE status = 'completed'), 0)::bigint AS rounding,
    coalesce(sum(total) FILTER (WHERE status = 'completed'), 0)::bigint AS total,
    count(*) FILTER (WHERE status = 'voided')::bigint AS voided,
    coalesce(sum(total) FILTER (WHERE status = 'voided'), 0)::bigint AS voided_total
FROM sale WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id AND business_date = @business_date;

-- name: DayPaymentMethods :many
SELECT p.method, count(*)::bigint AS payments, sum(p.amount)::bigint AS amount
FROM sale sa JOIN payment p ON p.tenant_id = sa.tenant_id AND p.sale_id = sa.id
WHERE sa.tenant_id = @tenant_id AND sa.outlet_id = @outlet_id AND sa.business_date = @business_date
  AND sa.status = 'completed' AND p.status = 'confirmed'
GROUP BY p.method ORDER BY p.method;

-- name: DayDiscounts :one
SELECT count(*)::bigint AS discounts, coalesce(sum(d.amount), 0)::bigint AS amount
FROM sale sa JOIN sale_discount d ON d.tenant_id = sa.tenant_id AND d.sale_id = sa.id
WHERE sa.tenant_id = @tenant_id AND sa.outlet_id = @outlet_id AND sa.business_date = @business_date AND sa.status = 'completed';

-- name: DayCashMovements :many
SELECT kind, count(*)::bigint AS movements, coalesce(sum(amount), 0)::bigint AS amount
FROM cash_movement
WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id AND business_date = @business_date
GROUP BY kind ORDER BY kind;

-- name: DayFlags :many
-- Flags raised about the day's sales, voids, shifts and cash movements, by code.
SELECT f.code, count(*)::bigint AS flags
FROM flag f
WHERE f.tenant_id = @tenant_id AND f.outlet_id = @outlet_id AND (
       f.target_id IN (SELECT sa.id FROM sale sa WHERE sa.tenant_id = @tenant_id AND sa.outlet_id = @outlet_id AND sa.business_date = @business_date)
    OR f.target_id IN (SELECT v.id FROM void v WHERE v.tenant_id = @tenant_id AND v.outlet_id = @outlet_id AND v.business_date = @business_date)
    OR f.target_id IN (SELECT sh.id FROM shift sh WHERE sh.tenant_id = @tenant_id AND sh.outlet_id = @outlet_id AND sh.business_date = @business_date)
    OR f.target_id IN (SELECT m.id FROM cash_movement m WHERE m.tenant_id = @tenant_id AND m.outlet_id = @outlet_id AND m.business_date = @business_date))
GROUP BY f.code ORDER BY f.code;

-- Sales over a range of business dates (B2.6). Completed sales only, as in the day report.

-- name: SalesByDay :many
-- Every date in the range, days without sales included.
SELECT d::date AS business_date,
    count(sa.id)::bigint AS sales,
    coalesce(sum(sa.subtotal), 0)::bigint AS subtotal,
    coalesce(sum(sa.discount_total), 0)::bigint AS discount_total,
    coalesce(sum(sa.service_charge), 0)::bigint AS service_charge,
    coalesce(sum(sa.tax), 0)::bigint AS tax,
    coalesce(sum(sa.rounding_amount), 0)::bigint AS rounding,
    coalesce(sum(sa.total), 0)::bigint AS total
FROM generate_series(@from_date::date, @to_date::date, interval '1 day') AS d
LEFT JOIN sale sa ON sa.tenant_id = @tenant_id AND sa.outlet_id = @outlet_id AND sa.business_date = d::date AND sa.status = 'completed'
GROUP BY d ORDER BY d;

-- name: SalesByItem :many
-- Per variant sold, named as in the catalog now. A line's total is its gross less its own discount
-- and its share of the bill discount, so the rows' net adds up to the sales' net.
SELECT l.variant_id, v.item_id, i.name AS item_name, v.name AS variant_name,
    sum(l.quantity)::bigint AS quantity,
    sum(l.line_total + l.line_discount + l.allocated_bill_discount)::bigint AS gross,
    sum(l.line_discount + l.allocated_bill_discount)::bigint AS discounts,
    sum(l.line_total)::bigint AS net
FROM sale sa
JOIN sale_line l ON l.tenant_id = sa.tenant_id AND l.sale_id = sa.id
JOIN variant v ON v.tenant_id = l.tenant_id AND v.id = l.variant_id
JOIN item i ON i.tenant_id = v.tenant_id AND i.id = v.item_id
WHERE sa.tenant_id = @tenant_id AND sa.outlet_id = @outlet_id AND sa.status = 'completed'
  AND sa.business_date BETWEEN @from_date::date AND @to_date::date
GROUP BY l.variant_id, v.item_id, i.name, v.name
ORDER BY net DESC, i.name, v.name, l.variant_id;

-- name: SalesByPaymentMethod :many
SELECT p.method, count(*)::bigint AS payments, sum(p.amount)::bigint AS amount
FROM sale sa JOIN payment p ON p.tenant_id = sa.tenant_id AND p.sale_id = sa.id
WHERE sa.tenant_id = @tenant_id AND sa.outlet_id = @outlet_id AND sa.status = 'completed' AND p.status = 'confirmed'
  AND sa.business_date BETWEEN @from_date::date AND @to_date::date
GROUP BY p.method ORDER BY p.method;
