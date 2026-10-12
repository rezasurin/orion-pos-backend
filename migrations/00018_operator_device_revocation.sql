-- An operator can revoke a device (BACKEND_PLAN.md 6.2.1, task B2.8). Operators are not users, so the
-- revocation records which operator did it in a column of its own; exactly one of the two is set on
-- a revoked device.

-- +goose Up
ALTER TABLE device ADD COLUMN revoked_by_operator_id uuid REFERENCES operator (id);
ALTER TABLE device DROP CONSTRAINT device_check;
ALTER TABLE device ADD CONSTRAINT device_revoked_by CHECK (
    (revoked_at IS NULL AND revoked_by IS NULL AND revoked_by_operator_id IS NULL)
    OR (revoked_at IS NOT NULL AND (revoked_by IS NULL) <> (revoked_by_operator_id IS NULL)));

-- +goose Down
ALTER TABLE device DROP CONSTRAINT device_revoked_by;
-- An operator's revocation has no user to put in revoked_by; going back loses who revoked it.
UPDATE device SET revoked_at = NULL WHERE revoked_by IS NULL;
ALTER TABLE device DROP COLUMN revoked_by_operator_id;
ALTER TABLE device ADD CONSTRAINT device_check CHECK ((revoked_at IS NULL) = (revoked_by IS NULL));
