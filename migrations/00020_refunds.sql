-- Refunds (BACKEND_PLAN.md 6.4.5, task B2.5): money given back for some or all of a completed sale,
-- recorded on the day it happens. The sale stays as rung up. The tablet decides the amounts; the
-- server records them as sent and flags a refund that goes beyond what was sold or paid.

-- +goose Up
CREATE TABLE refund (
    id            uuid PRIMARY KEY, -- the id of the refund.issued event
    tenant_id     uuid NOT NULL REFERENCES tenant (id),
    outlet_id     uuid NOT NULL,
    sale_id       uuid NOT NULL,
    shift_id      uuid NOT NULL, -- the shift it happened in: where cash left the drawer
    staff_id      uuid NOT NULL,
    approved_by   uuid,
    method        text NOT NULL CHECK (method IN ('cash', 'qris_manual', 'qris_dynamic', 'ewallet', 'card_manual')),
    amount        bigint NOT NULL CHECK (amount BETWEEN 0 AND 1000000000000), -- the sum of its lines
    reason        text NOT NULL CHECK (length(btrim(reason)) > 0 AND length(reason) <= 200),
    device_time   timestamptz NOT NULL,
    received_at   timestamptz NOT NULL,
    business_date date NOT NULL,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, sale_id) REFERENCES sale (tenant_id, id),
    FOREIGN KEY (tenant_id, shift_id) REFERENCES shift (tenant_id, id),
    FOREIGN KEY (tenant_id, staff_id) REFERENCES staff (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES staff (tenant_id, id)
);

-- What came back of each sale line.
CREATE TABLE refund_line (
    tenant_id    uuid NOT NULL,
    refund_id    uuid NOT NULL,
    sale_line_id uuid NOT NULL,
    quantity     integer NOT NULL CHECK (quantity BETWEEN 1 AND 10000),
    amount       bigint NOT NULL CHECK (amount BETWEEN 0 AND 1000000000000),
    PRIMARY KEY (tenant_id, refund_id, sale_line_id),
    FOREIGN KEY (tenant_id, refund_id) REFERENCES refund (tenant_id, id),
    FOREIGN KEY (tenant_id, sale_line_id) REFERENCES sale_line (tenant_id, id)
);

-- The refunds of a sale (the caps, the sale detail), of a shift (the drawer) and of an outlet's day
-- (the reports). refund_line is read through its refund, by the primary key.
CREATE INDEX refund_sale_idx ON refund (tenant_id, sale_id);
CREATE INDEX refund_shift_idx ON refund (tenant_id, shift_id);
CREATE INDEX refund_day_idx ON refund (tenant_id, outlet_id, business_date);

ALTER TABLE flag DROP CONSTRAINT flag_target_type_check;
ALTER TABLE flag ADD CONSTRAINT flag_target_type_check CHECK (target_type IN ('sale', 'void', 'shift', 'cash_movement', 'refund'));

ALTER TABLE refund ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON refund TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON refund TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE refund_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON refund_line TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON refund_line TO orion_platform USING (true) WITH CHECK (true);

-- Insert-only, like the rest of the sales tables.
GRANT SELECT, INSERT ON refund, refund_line TO orion_app, orion_platform;

-- +goose Down
DELETE FROM flag WHERE target_type = 'refund';
ALTER TABLE flag DROP CONSTRAINT flag_target_type_check;
ALTER TABLE flag ADD CONSTRAINT flag_target_type_check CHECK (target_type IN ('sale', 'void', 'shift', 'cash_movement'));
DROP TABLE refund_line;
DROP TABLE refund;
