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

-- name: GetMemberAccess :many
-- One member's role permissions per outlet, in one query. Zero rows means the user is not a
-- member. Inactive staff hold no roles.
SELECT m.is_owner, sor.outlet_id, rp.permission
FROM tenant_member m
LEFT JOIN staff s ON s.tenant_id = m.tenant_id AND s.user_id = m.user_id AND s.active
LEFT JOIN staff_outlet_role sor ON sor.tenant_id = s.tenant_id AND sor.staff_id = s.id
LEFT JOIN role_permission rp ON rp.tenant_id = sor.tenant_id AND rp.role_id = sor.role_id
WHERE m.tenant_id = @tenant_id AND m.user_id = @user_id;

-- name: InsertRole :exec
INSERT INTO role (id, tenant_id, name, is_system) VALUES (@id, @tenant_id, @name, @is_system);

-- name: InsertRolePermissions :exec
-- COPY is not allowed into tables with row-level security, so bulk inserts unnest an array.
INSERT INTO role_permission (tenant_id, role_id, permission)
SELECT @tenant_id::uuid, @role_id::uuid, unnest(@permissions::text[]);

-- name: ListRoles :many
SELECT * FROM role WHERE tenant_id = @tenant_id ORDER BY is_system DESC, name;

-- name: ListRolesByIDs :many
SELECT * FROM role WHERE tenant_id = @tenant_id AND id = ANY(@role_ids::uuid[]);

-- name: ListRolePermissions :many
-- Permissions for many roles at once, assembled in Go: two queries however many roles there are.
SELECT role_id, permission FROM role_permission
WHERE tenant_id = @tenant_id AND role_id = ANY(@role_ids::uuid[])
ORDER BY role_id, permission;

-- name: InsertStaff :exec
INSERT INTO staff (id, tenant_id, user_id, display_name, pin_hash, pin_rotated_at)
VALUES (@id, @tenant_id, sqlc.narg(user_id), @display_name, sqlc.narg(pin_hash), sqlc.narg(pin_rotated_at));

-- name: GetStaff :one
SELECT s.id, s.user_id, s.display_name, s.pin_hash, s.pin_rotated_at, s.active, s.created_at,
       coalesce(m.is_owner, false)::boolean AS is_owner
FROM staff s
LEFT JOIN tenant_member m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id
WHERE s.tenant_id = @tenant_id AND s.id = @id;

-- name: GetStaffForUpdate :one
SELECT s.id, s.user_id, s.display_name, s.pin_hash, s.pin_rotated_at, s.active, s.created_at,
       coalesce(m.is_owner, false)::boolean AS is_owner
FROM staff s
LEFT JOIN tenant_member m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id
WHERE s.tenant_id = @tenant_id AND s.id = @id
FOR UPDATE OF s;

-- name: ListStaff :many
SELECT s.id, s.user_id, s.display_name, s.pin_hash, s.pin_rotated_at, s.active, s.created_at,
       coalesce(m.is_owner, false)::boolean AS is_owner
FROM staff s
LEFT JOIN tenant_member m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id
WHERE s.tenant_id = @tenant_id AND s.id > @after
ORDER BY s.id
LIMIT @page_size;

-- name: ListStaffOutletRoles :many
-- The outlet roles of a page of staff, in one query.
SELECT staff_id, outlet_id, role_id FROM staff_outlet_role
WHERE tenant_id = @tenant_id AND staff_id = ANY(@staff_ids::uuid[])
ORDER BY staff_id, outlet_id, role_id;

-- name: UpdateStaff :exec
UPDATE staff SET display_name = @display_name, active = @active
WHERE tenant_id = @tenant_id AND id = @id;

-- name: SetStaffPIN :exec
UPDATE staff SET pin_hash = @pin_hash, pin_rotated_at = @now
WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertStaffOutletRoles :exec
INSERT INTO staff_outlet_role (tenant_id, staff_id, outlet_id, role_id)
SELECT @tenant_id::uuid, @staff_id::uuid, o.id, r.id
FROM unnest(@outlet_ids::uuid[]) WITH ORDINALITY AS o(id, n)
JOIN unnest(@role_ids::uuid[]) WITH ORDINALITY AS r(id, n) ON r.n = o.n
ON CONFLICT DO NOTHING;

-- name: DeleteStaffOutletRoles :exec
DELETE FROM staff_outlet_role
WHERE tenant_id = @tenant_id AND staff_id = @staff_id
  AND (outlet_id, role_id) IN (
      SELECT o.id, r.id
      FROM unnest(@outlet_ids::uuid[]) WITH ORDINALITY AS o(id, n)
      JOIN unnest(@role_ids::uuid[]) WITH ORDINALITY AS r(id, n) ON r.n = o.n
  );

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
