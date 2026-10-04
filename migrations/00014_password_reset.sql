-- Password reset (BACKEND_PLAN.md section 6.1.2).
--
-- A reset token is minted by the worker, so no secret waits in the job queue, and consumed by the
-- API. Like an email verification link it names one of the user's tenants, so the lookup runs
-- inside that tenant's row-level security; the user's password is global, so one reset covers every
-- business they belong to.

-- +goose Up
CREATE TABLE password_reset (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL REFERENCES tenant (id),
    user_id    uuid NOT NULL REFERENCES user_account (id),
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    UNIQUE (tenant_id, token_hash)
);

-- Using a reset retires the user's other open ones.
CREATE INDEX password_reset_user_idx ON password_reset (tenant_id, user_id);

ALTER TABLE password_reset ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON password_reset TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON password_reset TO orion_platform USING (true) WITH CHECK (true);

-- The app role reads reset links (the function below does the consuming); it never writes them.
GRANT SELECT ON password_reset TO orion_app;
GRANT SELECT, INSERT, DELETE ON password_reset TO orion_platform;

-- When the password last changed. A refresh token issued before it is dead, in every business the
-- user belongs to, so resetting a password signs out whoever else had the old one.
ALTER TABLE user_account ADD COLUMN password_changed_at timestamptz;
GRANT SELECT (password_changed_at) ON user_account TO orion_app;

-- The app role cannot write password_hash directly (it never could), and a reset does not change
-- that: the one way to change a password is this function, which needs the secret from the emailed
-- link. It takes the secret, not its hash, and hashes it here, because the app role can read the
-- stored hashes; so even arbitrary SQL as the app role cannot take over an account without a link
-- that was emailed to its owner. The link names the tenant, which must be the one the caller is
-- working in, and the user must still be a member of it. Returns the user id, or NULL for any
-- unusable link. p_now is the caller's clock, as everywhere else expiry is decided.
-- +goose StatementBegin
CREATE FUNCTION auth_reset_password(p_tenant_id uuid, p_secret bytea, p_password_hash text, p_now timestamptz)
    RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public, pg_temp
AS $$
DECLARE
    r password_reset%ROWTYPE;
BEGIN
    IF p_tenant_id IS DISTINCT FROM current_tenant_id() THEN
        RETURN NULL;
    END IF;
    SELECT * INTO r FROM password_reset
    WHERE tenant_id = p_tenant_id AND token_hash = sha256(p_secret)
    FOR UPDATE;
    IF NOT FOUND OR r.used_at IS NOT NULL OR r.expires_at <= p_now THEN
        RETURN NULL;
    END IF;
    PERFORM 1 FROM tenant_member WHERE tenant_id = p_tenant_id AND user_id = r.user_id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    -- The new password retires every open link of the user, this one included.
    UPDATE password_reset SET used_at = p_now
    WHERE tenant_id = p_tenant_id AND user_id = r.user_id AND used_at IS NULL;
    -- password_changed_at uses the database clock, like refresh_token.created_at, so they compare.
    UPDATE user_account SET password_hash = p_password_hash, password_changed_at = now()
    WHERE id = r.user_id;
    RETURN r.user_id;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_reset_password(uuid, bytea, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_reset_password(uuid, bytea, text, timestamptz) TO orion_app;

-- +goose Down
DROP FUNCTION auth_reset_password(uuid, bytea, text, timestamptz);
ALTER TABLE user_account DROP COLUMN password_changed_at;
DROP TABLE password_reset;
