-- Shifts, cash movements, sales, voids and the review flags the projectors raise (BACKEND_PLAN.md
-- sections 5.1 and 6.4). All of it is written only by projecting events a device pushed, and
-- nothing a device did is ever rewritten: the app role may insert, and update only the one column
-- (sale.status) and the shift close columns that the protocol itself changes.

-- +goose Up
CREATE TABLE shift (
    id            uuid PRIMARY KEY, -- the id of the shift.opened event
    tenant_id     uuid NOT NULL REFERENCES tenant (id),
    outlet_id     uuid NOT NULL,
    device_id     uuid NOT NULL,
    opened_by     uuid NOT NULL,
    opened_at     timestamptz NOT NULL, -- device_time of the opening
    opening_cash  bigint NOT NULL CHECK (opening_cash BETWEEN 0 AND 1000000000000),
    business_date date NOT NULL,
    received_at   timestamptz NOT NULL,
    closed_by     uuid,
    closed_at     timestamptz,
    counted_cash  bigint CHECK (counted_cash BETWEEN 0 AND 1000000000000),
    close_event_id uuid,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, device_id) REFERENCES device (tenant_id, id),
    FOREIGN KEY (tenant_id, opened_by) REFERENCES staff (tenant_id, id),
    FOREIGN KEY (tenant_id, closed_by) REFERENCES staff (tenant_id, id),
    -- A shift is open or closed with everything that closing records; never half of it. Expected
    -- cash is not stored: it is derived in the reports from the rows below, so a late event can
    -- never leave a stale number behind.
    CHECK ((closed_at IS NULL) = (closed_by IS NULL) AND (closed_at IS NULL) = (counted_cash IS NULL)
           AND (closed_at IS NULL) = (close_event_id IS NULL))
);

CREATE TABLE cash_movement (
    id            uuid PRIMARY KEY, -- the id of the cash.movement event
    tenant_id     uuid NOT NULL REFERENCES tenant (id),
    outlet_id     uuid NOT NULL,
    shift_id      uuid NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('pay_in', 'pay_out', 'no_sale')),
    amount        bigint NOT NULL CHECK (amount BETWEEN 0 AND 1000000000000),
    reason        text NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    staff_id      uuid NOT NULL,
    device_time   timestamptz NOT NULL,
    received_at   timestamptz NOT NULL,
    business_date date NOT NULL,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, shift_id) REFERENCES shift (tenant_id, id),
    FOREIGN KEY (tenant_id, staff_id) REFERENCES staff (tenant_id, id),
    CHECK (kind <> 'no_sale' OR amount = 0)
);

-- A completed sale as the device recorded it: the amounts are what the customer was charged, not
-- what the server would have calculated. A sale whose numbers do not recompute is flagged, never
-- altered. Monetary columns never change; status moves completed -> voided only through a void row.
CREATE TABLE sale (
    id                  uuid PRIMARY KEY, -- the id of the sale.completed event
    tenant_id           uuid NOT NULL REFERENCES tenant (id),
    outlet_id           uuid NOT NULL,
    device_id           uuid NOT NULL,
    shift_id            uuid NOT NULL,
    staff_id            uuid NOT NULL,
    receipt_number      text NOT NULL, -- {outlet code}-{device code}-{counter}, as shown on the receipt
    receipt_device_code integer NOT NULL CHECK (receipt_device_code >= 1),
    receipt_counter     bigint NOT NULL CHECK (receipt_counter >= 1),
    device_time         timestamptz NOT NULL,
    received_at         timestamptz NOT NULL,
    business_date       date NOT NULL,
    pricing_version     integer NOT NULL CHECK (pricing_version >= 1),
    -- The calculation settings the device used, so the sale can be recomputed whatever the outlet's
    -- settings are now.
    pricing             jsonb NOT NULL,
    catalog_seq         bigint NOT NULL CHECK (catalog_seq >= 0),
    subtotal            bigint NOT NULL CHECK (subtotal BETWEEN 0 AND 1000000000000),
    discount_total      bigint NOT NULL CHECK (discount_total BETWEEN 0 AND 1000000000000),
    service_charge      bigint NOT NULL CHECK (service_charge BETWEEN 0 AND 1000000000000),
    tax                 bigint NOT NULL CHECK (tax BETWEEN 0 AND 1000000000000),
    tax_included        boolean NOT NULL,
    rounding_amount     bigint NOT NULL CHECK (rounding_amount BETWEEN -1000000 AND 1000000),
    total               bigint NOT NULL CHECK (total BETWEEN 0 AND 1000000000000),
    status              text NOT NULL DEFAULT 'completed' CHECK (status IN ('completed', 'voided')),
    UNIQUE (tenant_id, id),
    CONSTRAINT sale_receipt_unique UNIQUE (tenant_id, outlet_id, receipt_device_code, receipt_counter),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, device_id) REFERENCES device (tenant_id, id),
    FOREIGN KEY (tenant_id, shift_id) REFERENCES shift (tenant_id, id),
    FOREIGN KEY (tenant_id, staff_id) REFERENCES staff (tenant_id, id)
);

CREATE TABLE sale_line (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenant (id),
    sale_id                 uuid NOT NULL,
    line_no                 integer NOT NULL CHECK (line_no >= 0),
    variant_id              uuid NOT NULL,
    name_snapshot           text NOT NULL CHECK (length(name_snapshot) BETWEEN 1 AND 200),
    unit_price              bigint NOT NULL CHECK (unit_price BETWEEN 0 AND 1000000000),
    quantity                integer NOT NULL CHECK (quantity BETWEEN 1 AND 10000),
    line_discount           bigint NOT NULL CHECK (line_discount >= 0),
    allocated_bill_discount bigint NOT NULL CHECK (allocated_bill_discount >= 0),
    line_total              bigint NOT NULL CHECK (line_total >= 0),
    UNIQUE (tenant_id, id),
    -- Also how a sale's lines are read: one range scan.
    UNIQUE (tenant_id, sale_id, line_no),
    FOREIGN KEY (tenant_id, sale_id) REFERENCES sale (tenant_id, id),
    FOREIGN KEY (tenant_id, variant_id) REFERENCES variant (tenant_id, id)
);

CREATE TABLE sale_line_modifier (
    tenant_id     uuid NOT NULL,
    sale_line_id  uuid NOT NULL,
    position      integer NOT NULL CHECK (position >= 0),
    modifier_id   uuid NOT NULL,
    name_snapshot text NOT NULL CHECK (length(name_snapshot) BETWEEN 1 AND 200),
    price_delta   bigint NOT NULL CHECK (price_delta BETWEEN -1000000000 AND 1000000000),
    PRIMARY KEY (tenant_id, sale_line_id, position),
    FOREIGN KEY (tenant_id, sale_line_id) REFERENCES sale_line (tenant_id, id),
    FOREIGN KEY (tenant_id, modifier_id) REFERENCES modifier (tenant_id, id)
);

CREATE TABLE sale_discount (
    id           uuid PRIMARY KEY,
    tenant_id    uuid NOT NULL REFERENCES tenant (id),
    sale_id      uuid NOT NULL,
    sale_line_id uuid, -- null for a discount on the whole bill
    kind         text NOT NULL CHECK (kind IN ('percent', 'amount')),
    value        bigint NOT NULL CHECK (value >= 0), -- basis points for a percent, rupiah for an amount
    amount       bigint NOT NULL CHECK (amount >= 0),
    reason       text NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    approved_by  uuid,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, sale_id) REFERENCES sale (tenant_id, id),
    FOREIGN KEY (tenant_id, sale_line_id) REFERENCES sale_line (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES staff (tenant_id, id)
);

CREATE TABLE payment (
    id        uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenant (id),
    sale_id   uuid NOT NULL,
    method    text NOT NULL CHECK (method IN ('cash', 'qris_manual', 'qris_dynamic', 'ewallet', 'card_manual')),
    amount    bigint NOT NULL CHECK (amount BETWEEN 0 AND 1000000000000), -- applied to the bill
    tendered  bigint CHECK (tendered >= 0), -- cash handed over; amount + change
    change    bigint CHECK (change >= 0),
    reference text NOT NULL DEFAULT '' CHECK (length(reference) <= 100),
    status    text NOT NULL DEFAULT 'confirmed' CHECK (status IN ('pending', 'confirmed', 'failed')),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, sale_id) REFERENCES sale (tenant_id, id)
);

CREATE TABLE void (
    id            uuid PRIMARY KEY, -- the id of the sale.voided event
    tenant_id     uuid NOT NULL REFERENCES tenant (id),
    outlet_id     uuid NOT NULL,
    sale_id       uuid NOT NULL,
    shift_id      uuid NOT NULL, -- the shift the void happened in: where the refund left the drawer
    staff_id      uuid NOT NULL,
    approved_by   uuid,
    reason        text NOT NULL CHECK (length(btrim(reason)) > 0 AND length(reason) <= 200),
    device_time   timestamptz NOT NULL,
    received_at   timestamptz NOT NULL,
    business_date date NOT NULL,
    UNIQUE (tenant_id, id),
    CONSTRAINT void_sale_unique UNIQUE (tenant_id, sale_id), -- a sale is voided once
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, sale_id) REFERENCES sale (tenant_id, id),
    FOREIGN KEY (tenant_id, shift_id) REFERENCES shift (tenant_id, id),
    FOREIGN KEY (tenant_id, staff_id) REFERENCES staff (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES staff (tenant_id, id)
);

-- Things the projectors accepted but want a person to look at: totals that do not recompute, a
-- sale or void by someone without the permission, a price that was out of date. The money has
-- already changed hands, so the event stands and the flag is the review (section 5.1).
CREATE TABLE flag (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    outlet_id   uuid NOT NULL,
    device_id   uuid NOT NULL,
    event_id    uuid NOT NULL, -- the sync_inbox event that raised it
    target_type text NOT NULL CHECK (target_type IN ('sale', 'void', 'shift', 'cash_movement')),
    target_id   uuid NOT NULL,
    code        text NOT NULL CHECK (code ~ '^[a-z_]+$'),
    detail      jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id)
);

ALTER TABLE shift ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON shift TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON shift TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE cash_movement ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cash_movement TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON cash_movement TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE sale ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sale TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON sale TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE sale_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sale_line TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON sale_line TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE sale_line_modifier ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sale_line_modifier TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON sale_line_modifier TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE sale_discount ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sale_discount TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON sale_discount TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE payment ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON payment TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE void ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON void TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON void TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE flag ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON flag TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON flag TO orion_platform USING (true) WITH CHECK (true);

-- Insert-only, with the two places the protocol itself moves a row: a shift closes, and a sale is
-- voided. No DELETE anywhere.
GRANT SELECT, INSERT ON shift, cash_movement, sale, sale_line, sale_line_modifier, sale_discount, payment, void, flag
    TO orion_app, orion_platform;
GRANT UPDATE (closed_by, closed_at, counted_cash, close_event_id) ON shift TO orion_app;
GRANT UPDATE (status) ON sale TO orion_app;
GRANT UPDATE ON shift, sale TO orion_platform;

-- +goose Down
DROP TABLE flag;
DROP TABLE void;
DROP TABLE payment;
DROP TABLE sale_discount;
DROP TABLE sale_line_modifier;
DROP TABLE sale_line;
DROP TABLE sale;
DROP TABLE cash_movement;
DROP TABLE shift;
