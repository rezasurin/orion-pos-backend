-- All queries here run as orion_platform.

-- name: InsertOperator :exec
INSERT INTO operator (id, email, password_hash, totp_secret_enc) VALUES (@id, @email, @password_hash, @totp_secret_enc);

-- name: GetOperatorByEmail :one
SELECT * FROM operator WHERE email = @email;

-- name: GetOperator :one
SELECT * FROM operator WHERE id = @id;

-- name: GetOperatorForUpdate :one
SELECT * FROM operator WHERE id = @id FOR UPDATE;

-- name: CountOperators :one
SELECT count(*) FROM operator;

-- name: AdvanceTOTP :exec
-- Records the accepted time step and confirms enrolment on the first success.
UPDATE operator SET totp_last_step = @step, totp_confirmed_at = coalesce(totp_confirmed_at, @now::timestamptz)
WHERE id = @id;

-- name: InsertRecoveryCode :exec
INSERT INTO operator_recovery_code (id, operator_id, code_hash) VALUES (@id, @operator_id, @code_hash);

-- name: UseRecoveryCode :execrows
UPDATE operator_recovery_code SET used_at = @now
WHERE operator_id = @operator_id AND code_hash = @code_hash AND used_at IS NULL;

-- name: InsertAudit :exec
INSERT INTO platform_audit_log (id, operator_id, action, target_type, target_id, tenant_id, before, after, reason, ip, user_agent)
VALUES (@id, sqlc.narg(operator_id), @action, @target_type, sqlc.narg(target_id), sqlc.narg(tenant_id),
        sqlc.narg(before), sqlc.narg(after), @reason, sqlc.narg(ip), sqlc.narg(user_agent));

-- name: ListAudit :many
-- Newest first; the cursor is the id of the last row of the previous page.
SELECT id, operator_id, action, target_type, target_id, tenant_id, before, after, reason, coalesce(host(ip), '')::text AS ip, user_agent, created_at
FROM platform_audit_log
WHERE id < @before
ORDER BY id DESC
LIMIT @page_size;
