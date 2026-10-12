-- Indexes for the reports and the back-office sales list (BACKEND_PLAN.md section 4.12: each one
-- is here because a query reads that way). PostgreSQL does not index the referencing side of a
-- foreign key, so "the payments of a sale" and "the sales of a shift" would scan the whole table
-- without these. The load check (B1.11) runs EXPLAIN on each report against a busy week.

-- +goose Up
-- End of day: the day's completed sales of an outlet; the sales list pages through the same range.
CREATE INDEX sale_day_idx ON sale (tenant_id, outlet_id, business_date);
-- End of shift: the sales of a shift.
CREATE INDEX sale_shift_idx ON sale (tenant_id, shift_id);
-- The payments and discounts of one sale or of a day's sales, joined from the sale.
CREATE INDEX payment_sale_idx ON payment (tenant_id, sale_id);
CREATE INDEX sale_discount_sale_idx ON sale_discount (tenant_id, sale_id);
-- Voids recorded in a shift (where a refund left the drawer).
CREATE INDEX void_shift_idx ON void (tenant_id, shift_id);
-- Cash movements of a shift and of a day.
CREATE INDEX cash_movement_shift_idx ON cash_movement (tenant_id, shift_id);
CREATE INDEX cash_movement_day_idx ON cash_movement (tenant_id, outlet_id, business_date);
-- The shifts of an outlet's day.
CREATE INDEX shift_day_idx ON shift (tenant_id, outlet_id, business_date);
-- The flags raised about a sale, void or shift.
CREATE INDEX flag_target_idx ON flag (tenant_id, target_id);

-- +goose Down
DROP INDEX flag_target_idx;
DROP INDEX shift_day_idx;
DROP INDEX cash_movement_day_idx;
DROP INDEX cash_movement_shift_idx;
DROP INDEX void_shift_idx;
DROP INDEX sale_discount_sale_idx;
DROP INDEX payment_sale_idx;
DROP INDEX sale_shift_idx;
DROP INDEX sale_day_idx;
