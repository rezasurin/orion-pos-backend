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

-- Devices.

-- name: AllocateDeviceCode :one
-- Hands out the next code for an outlet. The first call for an outlet inserts the counter; later
-- calls take its row lock (after the tenant lock, in the fixed order) and increment it.
INSERT INTO device_code_counter (outlet_id, tenant_id, next_code)
VALUES (@outlet_id, @tenant_id, 2)
ON CONFLICT (outlet_id) DO UPDATE SET next_code = device_code_counter.next_code + 1
RETURNING (next_code - 1)::integer AS code;

-- name: InsertDevice :exec
INSERT INTO device (id, tenant_id, outlet_id, device_code, name, secret_hash, paired_by)
VALUES (@id, @tenant_id, @outlet_id, @device_code, @name, @secret_hash, @paired_by);

-- name: GetDevice :one
SELECT * FROM device WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetDeviceForUpdate :one
SELECT * FROM device WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListDevices :many
-- Non-owners see only the outlets where they may manage devices.
SELECT * FROM device
WHERE tenant_id = @tenant_id AND id > @after
  AND (@all_outlets::boolean OR outlet_id = ANY(@outlet_ids::uuid[]))
ORDER BY id
LIMIT @page_size;

-- name: RevokeDevice :exec
UPDATE device SET revoked_at = @now, revoked_by = @revoked_by
WHERE tenant_id = @tenant_id AND id = @id AND revoked_at IS NULL;

-- name: RecordDeviceContact :exec
-- Stamps a token exchange: when the device was seen, its app version and how far its clock is off.
UPDATE device SET last_seen_at = @now, app_version = sqlc.narg(app_version), clock_skew_ms = sqlc.narg(clock_skew_ms)
WHERE tenant_id = @tenant_id AND id = @id;

-- name: TouchDevice :exec
-- Keeps last_seen_at fresh without a write on every request: the caller passes a stale_before a
-- minute in the past, so a device seen more recently than that is left alone.
UPDATE device SET last_seen_at = @now
WHERE tenant_id = @tenant_id AND id = @id AND (last_seen_at IS NULL OR last_seen_at < @stale_before);

-- name: ListRosterStaff :many
-- The staff a device at this outlet shows on its lock screen: active staff assigned to the outlet,
-- and owners (who may act anywhere).
SELECT s.id, s.display_name, s.pin_hash, coalesce(m.is_owner, false)::boolean AS is_owner
FROM staff s
LEFT JOIN tenant_member m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id
WHERE s.tenant_id = @tenant_id AND s.active
  AND (coalesce(m.is_owner, false) OR EXISTS (
        SELECT 1 FROM staff_outlet_role sor
        WHERE sor.tenant_id = s.tenant_id AND sor.staff_id = s.id AND sor.outlet_id = @outlet_id))
ORDER BY s.display_name, s.id;

-- name: ListRosterPermissions :many
-- Everyone's permissions at the outlet in one query.
SELECT sor.staff_id, rp.permission
FROM staff_outlet_role sor
JOIN role_permission rp ON rp.tenant_id = sor.tenant_id AND rp.role_id = sor.role_id
WHERE sor.tenant_id = @tenant_id AND sor.outlet_id = @outlet_id
ORDER BY sor.staff_id, rp.permission;

-- name: CountActiveDevices :one
SELECT count(*) FROM device WHERE tenant_id = @tenant_id AND revoked_at IS NULL;

-- name: CountActiveStaff :one
SELECT count(*) FROM staff WHERE tenant_id = @tenant_id AND active;

-- Used while projecting synced events (internal/sync). They run inside the event's transaction.

-- name: GetDeviceForShare :one
-- Holding a share lock on the device until the event commits makes revocation (an UPDATE) wait for
-- events in flight, so nothing commits after a revoke has returned.
SELECT * FROM device WHERE tenant_id = @tenant_id AND id = @id FOR SHARE;

-- name: ListStaffPermissionsAt :many
-- What one staff member may do at an outlet.
SELECT rp.permission
FROM staff_outlet_role sor
JOIN role_permission rp ON rp.tenant_id = sor.tenant_id AND rp.role_id = sor.role_id
WHERE sor.tenant_id = @tenant_id AND sor.staff_id = @staff_id AND sor.outlet_id = @outlet_id
ORDER BY rp.permission;

-- name: RecordDeviceSync :exec
-- Stamps a push or pull: when the device last synced, its app version, how far its clock is off and,
-- when it reported its outbox, how much is still waiting there.
UPDATE device
SET last_sync_at = @now, last_seen_at = @now,
    app_version = coalesce(sqlc.narg(app_version), app_version),
    clock_skew_ms = coalesce(sqlc.narg(clock_skew_ms), clock_skew_ms),
    unsynced_events = CASE WHEN sqlc.narg(unsynced_events)::integer IS NULL THEN unsynced_events ELSE sqlc.narg(unsynced_events)::integer END,
    oldest_unsynced_at = CASE WHEN sqlc.narg(unsynced_events)::integer IS NULL THEN oldest_unsynced_at
                              WHEN sqlc.narg(unsynced_events)::integer = 0 THEN NULL
                              ELSE sqlc.narg(oldest_unsynced_at)::timestamptz END,
    health_reported_at = CASE WHEN sqlc.narg(unsynced_events)::integer IS NULL THEN health_reported_at ELSE @now END
WHERE tenant_id = @tenant_id AND id = @id;

-- The POS pull: roster entries for the staff whose records changed.

-- name: ListRosterCandidates :many
-- For each staff id: whether the person belongs on this outlet's roster (active, and an owner or
-- assigned to the outlet). Those who do not are reported as removed.
SELECT s.id, s.display_name, s.pin_hash, coalesce(m.is_owner, false)::boolean AS is_owner,
       (s.active AND (coalesce(m.is_owner, false) OR EXISTS (
            SELECT 1 FROM staff_outlet_role sor
            WHERE sor.tenant_id = s.tenant_id AND sor.staff_id = s.id AND sor.outlet_id = @outlet_id)))::boolean AS on_roster
FROM staff s
LEFT JOIN tenant_member m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id
WHERE s.tenant_id = @tenant_id AND s.id = ANY(@staff_ids::uuid[])
ORDER BY s.display_name, s.id;

-- name: ListPermissionsOfStaffAt :many
SELECT sor.staff_id, rp.permission
FROM staff_outlet_role sor
JOIN role_permission rp ON rp.tenant_id = sor.tenant_id AND rp.role_id = sor.role_id
WHERE sor.tenant_id = @tenant_id AND sor.outlet_id = @outlet_id AND sor.staff_id = ANY(@staff_ids::uuid[])
ORDER BY sor.staff_id, rp.permission;

-- name: InsertTermsAcceptance :exec
INSERT INTO terms_acceptance (tenant_id, user_id, version, accepted_at)
VALUES (@tenant_id, @user_id, @version, @accepted_at);
