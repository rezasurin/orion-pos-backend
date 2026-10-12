-- The stock ledger (BACKEND_PLAN.md 6.6.4, task B3.3; ADR 0006, ADR 0009; BUSINESS_RULES.md BR-STK,
-- BR-CST). Stock on hand is the sum of movements per item and outlet; stock_balance is that sum kept
-- in the same transaction as each movement, with the totals the weighted average cost comes from.

-- +goose Up
CREATE TABLE stock_movement (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    outlet_id       uuid NOT NULL,
    stock_item_id   uuid NOT NULL,
    -- Posting order per item and outlet, taken while the balance row is locked. The average cost
    -- depends on the order movements were posted in, so a rebuild replays this order.
    seq             bigint NOT NULL CHECK (seq >= 1),
    kind            text NOT NULL CHECK (kind IN ('opening', 'purchase_receipt', 'supplier_return', 'sale_consumption', 'production_in',
                        'production_out', 'usage', 'waste', 'adjustment', 'opname_adjustment', 'transfer_out', 'transfer_in', 'reversal')),
    -- Thousandths of the item's base unit, signed (ADR 0009).
    quantity_scaled bigint NOT NULL CHECK (quantity_scaled <> 0 AND abs(quantity_scaled) <= 1000000000000000),
    -- Whole rupiah, signed like the quantity (BR-CST-03); 0 when nothing is known of the cost.
    value           bigint NOT NULL CHECK (abs(value) <= 1000000000000000),
    reason          text CHECK (length(reason) BETWEEN 1 AND 40),
    note            text CHECK (length(note) <= 500),
    staff_id        uuid NOT NULL, -- who posted it (every member who acts has a staff record)
    source_type     text CHECK (length(source_type) BETWEEN 1 AND 40),
    source_id       uuid,
    reverses_id     uuid,
    occurred_at     timestamptz NOT NULL, -- the effective time (BR-STK-05)
    business_date   date NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    -- Also the access path for rebuilding and for an item's history.
    UNIQUE (tenant_id, outlet_id, stock_item_id, seq),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, stock_item_id) REFERENCES stock_item (tenant_id, id),
    FOREIGN KEY (tenant_id, staff_id) REFERENCES staff (tenant_id, id),
    FOREIGN KEY (tenant_id, reverses_id) REFERENCES stock_movement (tenant_id, id),
    CHECK ((source_type IS NULL) = (source_id IS NULL)),
    CHECK ((kind = 'reversal') = (reverses_id IS NOT NULL)),
    -- Stock in, stock out, or either way.
    CHECK (CASE
        WHEN kind IN ('opening', 'purchase_receipt', 'production_in', 'transfer_in') THEN quantity_scaled > 0
        WHEN kind IN ('supplier_return', 'sale_consumption', 'production_out', 'usage', 'waste', 'transfer_out') THEN quantity_scaled < 0
        ELSE true END),
    CHECK (value = 0 OR sign(value) = sign(quantity_scaled))
);
-- A movement is reversed at most once.
CREATE UNIQUE INDEX stock_movement_reverses_idx ON stock_movement (tenant_id, reverses_id) WHERE reverses_id IS NOT NULL;

CREATE TABLE stock_balance (
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    outlet_id       uuid NOT NULL,
    stock_item_id   uuid NOT NULL,
    quantity_scaled bigint NOT NULL,
    value           bigint NOT NULL,
    -- The quantity and value the last time the quantity was above 0: the last known average cost,
    -- which consumption uses while the balance is negative (BR-CST-04). Null until then.
    cost_quantity   bigint CHECK (cost_quantity > 0),
    cost_value      bigint,
    seq             bigint NOT NULL CHECK (seq >= 0), -- the last movement's seq
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, outlet_id, stock_item_id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    FOREIGN KEY (tenant_id, stock_item_id) REFERENCES stock_item (tenant_id, id),
    CHECK ((cost_quantity IS NULL) = (cost_value IS NULL))
);

ALTER TABLE stock_movement ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_balance ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON stock_movement TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY tenant_isolation ON stock_balance TO orion_app USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON stock_movement TO orion_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_all ON stock_balance TO orion_platform USING (true) WITH CHECK (true);

-- The ledger is append-only for everyone (BR-STK-01, BR-GEN-05): a mistake is a reversal. Balances
-- are written by posting, and by the platform role when an operator rebuilds them.
GRANT SELECT, INSERT ON stock_movement TO orion_app;
GRANT SELECT ON stock_movement TO orion_platform;
GRANT SELECT, INSERT ON stock_balance TO orion_app, orion_platform;
GRANT UPDATE (quantity_scaled, value, cost_quantity, cost_value, seq, updated_at) ON stock_balance TO orion_app, orion_platform;

-- +goose Down
DROP TABLE stock_balance;
DROP TABLE stock_movement;
