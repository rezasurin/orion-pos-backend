-- name: InsertUser :exec
-- No RETURNING: orion_app may insert a user but only read members of its own tenant, and this
-- user is not a member yet. The caller generates the id.
INSERT INTO user_account (id, email, password_hash, email_verified_at, locale)
VALUES (@id, @email, @password_hash, sqlc.narg(email_verified_at), @locale);

-- name: InsertMember :exec
INSERT INTO tenant_member (tenant_id, user_id, is_owner)
VALUES (@tenant_id, @user_id, @is_owner);

-- name: GetUser :one
SELECT id, email, email_verified_at, locale, created_at FROM user_account WHERE id = @id;

-- name: GetMember :one
SELECT is_owner FROM tenant_member WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: InsertRefreshToken :exec
INSERT INTO refresh_token (id, tenant_id, user_id, family_id, token_hash, expires_at)
VALUES (@id, @tenant_id, @user_id, @family_id, @token_hash, @expires_at);

-- name: GetRefreshTokenForUpdate :one
SELECT * FROM refresh_token WHERE tenant_id = @tenant_id AND token_hash = @token_hash FOR UPDATE;

-- name: MarkRefreshTokenUsed :exec
UPDATE refresh_token SET used_at = @now WHERE id = @id;

-- name: RevokeRefreshFamily :exec
-- Rows are locked in id order so two concurrent revocations of one family cannot deadlock.
UPDATE refresh_token SET revoked_at = @now
WHERE id IN (
    SELECT t.id FROM refresh_token t
    WHERE t.tenant_id = @tenant_id AND t.family_id = @family_id AND t.revoked_at IS NULL
    ORDER BY t.id
    FOR UPDATE
);

-- name: GetVerificationForUpdate :one
SELECT * FROM email_verification WHERE tenant_id = @tenant_id AND token_hash = @token_hash FOR UPDATE;

-- name: MarkVerificationUsed :exec
UPDATE email_verification SET used_at = @now WHERE id = @id;

-- name: MarkEmailVerified :exec
UPDATE user_account SET email_verified_at = @now WHERE id = @id AND email_verified_at IS NULL;

-- Queries below run as orion_platform, from the worker.

-- name: InsertEmailVerification :exec
INSERT INTO email_verification (id, tenant_id, user_id, token_hash, expires_at)
VALUES (@id, @tenant_id, @user_id, @token_hash, @expires_at);

-- name: GetUserForEmail :one
SELECT id, email, email_verified_at, locale FROM user_account WHERE id = @id;

-- name: PurgeRefreshTokens :execrows
DELETE FROM refresh_token WHERE expires_at < @before;

-- name: PurgeEmailVerifications :execrows
DELETE FROM email_verification WHERE expires_at < @before;
