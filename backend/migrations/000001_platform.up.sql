-- Schema awal platform. ID dibuat oleh Go; jalankan sebagai migrator, bukan role target.
CREATE SCHEMA databrain;

CREATE TABLE databrain.security_guard (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton)
);
INSERT INTO databrain.security_guard VALUES (true);

CREATE TABLE databrain.users (
    id uuid PRIMARY KEY,
    username varchar(32) NOT NULL UNIQUE CHECK (username ~ '^[a-z][a-z0-9_]{2,31}$'),
    display_name varchar(80) NOT NULL CHECK (length(btrim(display_name)) > 0),
    password_hash text NOT NULL,
    is_platform_admin boolean NOT NULL DEFAULT false,
    is_active boolean NOT NULL DEFAULT true,
    security_version bigint NOT NULL DEFAULT 1 CHECK (security_version > 0),
    revision integer NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE databrain.groups (
    id uuid PRIMARY KEY,
    name varchar(64) NOT NULL CHECK (length(btrim(name)) > 0),
    is_active boolean NOT NULL DEFAULT true,
    revision integer NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX groups_name_lower_unique ON databrain.groups (lower(name));

CREATE TABLE databrain.memberships (
    group_id uuid NOT NULL REFERENCES databrain.groups(id) ON DELETE RESTRICT,
    user_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX memberships_by_user ON databrain.memberships(user_id, group_id);

CREATE TABLE databrain.sessions (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('anonymous', 'authenticated')),
    user_id uuid REFERENCES databrain.users(id) ON DELETE RESTRICT,
    security_version bigint,
    current_token_hash bytea NOT NULL CHECK (octet_length(current_token_hash) = 32),
    current_token_sealed bytea NOT NULL CHECK (octet_length(current_token_sealed) >= 30),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    last_rotated_at timestamptz NOT NULL DEFAULT now(),
    absolute_expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    reauth_required boolean NOT NULL DEFAULT false,
    CHECK (absolute_expires_at > created_at),
    CHECK ((kind = 'anonymous' AND user_id IS NULL AND security_version IS NULL)
        OR (kind = 'authenticated' AND user_id IS NOT NULL AND security_version IS NOT NULL AND security_version > 0))
);
CREATE INDEX sessions_by_user ON databrain.sessions(user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_cleanup ON databrain.sessions(absolute_expires_at);
CREATE TABLE databrain.session_tokens (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    session_id uuid NOT NULL REFERENCES databrain.sessions(id) ON DELETE CASCADE,
    valid_until timestamptz NOT NULL,
    UNIQUE (session_id, token_hash)
);
ALTER TABLE databrain.sessions ADD CONSTRAINT session_canonical_token_fk
    FOREIGN KEY (id, current_token_hash) REFERENCES databrain.session_tokens(session_id, token_hash)
    DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX session_tokens_by_session ON databrain.session_tokens(session_id);
CREATE INDEX session_tokens_cleanup ON databrain.session_tokens(valid_until);

CREATE TABLE databrain.login_windows (
    key_type text NOT NULL CHECK (key_type IN ('username', 'client')),
    key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    window_start timestamptz NOT NULL,
    attempts integer NOT NULL CHECK (attempts >= 0),
    PRIMARY KEY (key_type, key_hash, window_start)
);
CREATE INDEX login_windows_cleanup ON databrain.login_windows(window_start);

CREATE TABLE databrain.database_targets (
    id uuid PRIMARY KEY,
    name varchar(80) NOT NULL CHECK (length(btrim(name)) > 0),
    host varchar(253) NOT NULL CHECK (host = lower(host) AND length(host) > 0),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    database_name text NOT NULL CHECK (octet_length(database_name) BETWEEN 1 AND 63),
    username text NOT NULL CHECK (octet_length(username) BETWEEN 1 AND 63),
    password_sealed bytea NOT NULL CHECK (octet_length(password_sealed) >= 30),
    ssl_mode text NOT NULL CHECK (ssl_mode IN ('verify-full', 'disable')),
    is_active boolean NOT NULL DEFAULT true,
    config_revision integer NOT NULL DEFAULT 1 CHECK (config_revision > 0),
    created_by uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (host, port, database_name)
);
CREATE FUNCTION databrain.protect_target_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.host,NEW.port,NEW.database_name) IS DISTINCT FROM ROW(OLD.host,OLD.port,OLD.database_name) THEN
        RAISE EXCEPTION 'target identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_target_identity BEFORE UPDATE ON databrain.database_targets
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_target_identity();
CREATE TABLE databrain.database_grants (
    group_id uuid NOT NULL REFERENCES databrain.groups(id) ON DELETE RESTRICT,
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    access_level text NOT NULL CHECK (access_level IN ('view_only', 'view_edit')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, target_id)
);
CREATE INDEX grants_by_target ON databrain.database_grants(target_id, group_id);

CREATE TABLE databrain.migration_drafts (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    title varchar(120) NOT NULL CHECK (length(btrim(title)) > 0),
    sql text NOT NULL CHECK (octet_length(sql) BETWEEN 1 AND 131072),
    revision integer NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_by uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    updated_by uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (target_id, id)
);
CREATE INDEX drafts_by_target ON databrain.migration_drafts(target_id, updated_at DESC, id);

CREATE TABLE databrain.migration_attempts (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    draft_id uuid NOT NULL,
    draft_revision integer NOT NULL CHECK (draft_revision > 0),
    version bigint NOT NULL CHECK (version > 0),
    title_snapshot varchar(120) NOT NULL,
    target_name_snapshot varchar(80) NOT NULL,
    target_config_revision integer NOT NULL CHECK (target_config_revision > 0),
    sql_snapshot text NOT NULL CHECK (octet_length(sql_snapshot) BETWEEN 1 AND 131072),
    sql_sha256 bytea NOT NULL CHECK (octet_length(sql_sha256) = 32),
    executor_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    executor_username_snapshot varchar(32) NOT NULL,
    request_key uuid NOT NULL,
    state text NOT NULL CHECK (state IN ('reserved', 'running', 'succeeded', 'failed', 'unknown')),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    commit_started_at timestamptz,
    finished_at timestamptz,
    first_unknown_at timestamptz,
    failure_code text CHECK (failure_code IN ('CONNECTION_FAILED','ACCESS_REVOKED','SQL_ERROR','SQL_TIMEOUT',
        'LOCK_TIMEOUT','CANCELED','COMMIT_UNCERTAIN','OUTCOME_PERSISTENCE_FAILED','PROCESS_INTERRUPTED')),
    failure_message varchar(240),
    failure_sqlstate varchar(5),
    failure_statement_index integer CHECK (failure_statement_index BETWEEN 1 AND 50),
    outcome_evidence text CHECK (outcome_evidence IN
        ('commit_ack', 'rollback_ack', 'server_rollback', 'not_started', 'operator_commit', 'operator_rollback')),
    resolution_reason text,
    resolved_at timestamptz,
    resolved_by_operator varchar(120),
    FOREIGN KEY (target_id, draft_id) REFERENCES databrain.migration_drafts(target_id, id) ON DELETE RESTRICT,
    UNIQUE (target_id, id),
    UNIQUE (target_id, version),
    UNIQUE (target_id, executor_id, request_key),
    CHECK ((state IN ('failed','unknown') AND failure_code IS NOT NULL AND failure_message IS NOT NULL)
        OR (state IN ('reserved','running','succeeded') AND failure_code IS NULL AND failure_message IS NULL)),
    CHECK ((state = 'succeeded' AND finished_at IS NOT NULL AND outcome_evidence IS NOT NULL AND outcome_evidence IN ('commit_ack','operator_commit'))
        OR (state = 'failed' AND finished_at IS NOT NULL AND outcome_evidence IS NOT NULL AND outcome_evidence IN ('rollback_ack','server_rollback','not_started','operator_rollback'))
        OR (state IN ('reserved','running','unknown') AND finished_at IS NULL AND outcome_evidence IS NULL)),
    CHECK ((resolved_at IS NULL AND resolved_by_operator IS NULL AND resolution_reason IS NULL)
        OR (resolved_at IS NOT NULL AND resolved_by_operator IS NOT NULL AND resolution_reason IS NOT NULL AND length(btrim(resolution_reason)) > 0))
);
CREATE UNIQUE INDEX one_unresolved_attempt_per_target ON databrain.migration_attempts(target_id)
    WHERE state IN ('reserved', 'running', 'unknown');
CREATE UNIQUE INDEX one_success_per_draft_revision ON databrain.migration_attempts(draft_id, draft_revision)
    WHERE state = 'succeeded';
CREATE INDEX history_by_target ON databrain.migration_attempts(target_id, version DESC);

CREATE TABLE databrain.migration_streams (
    target_id uuid PRIMARY KEY REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    next_version bigint NOT NULL DEFAULT 1 CHECK (next_version > 0),
    active_attempt_id uuid,
    FOREIGN KEY (target_id, active_attempt_id) REFERENCES databrain.migration_attempts(target_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE FUNCTION databrain.protect_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'reserved' THEN RAISE EXCEPTION 'attempt must start reserved'; END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.id,NEW.target_id,NEW.draft_id,NEW.draft_revision,NEW.version,
           NEW.title_snapshot,NEW.target_name_snapshot,NEW.target_config_revision,
           NEW.sql_snapshot,NEW.sql_sha256,NEW.executor_id,NEW.executor_username_snapshot,
           NEW.request_key,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.target_id,OLD.draft_id,OLD.draft_revision,OLD.version,
           OLD.title_snapshot,OLD.target_name_snapshot,OLD.target_config_revision,
           OLD.sql_snapshot,OLD.sql_sha256,OLD.executor_id,OLD.executor_username_snapshot,
           OLD.request_key,OLD.created_at) THEN
        RAISE EXCEPTION 'execution snapshot is immutable';
    END IF;
    IF OLD.state IN ('succeeded','failed') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'terminal execution is immutable';
    END IF;
    IF NEW.state <> OLD.state AND NOT (
        (OLD.state = 'reserved' AND NEW.state IN ('running','failed','unknown')) OR
        (OLD.state = 'running' AND NEW.state IN ('succeeded','failed','unknown')) OR
        (OLD.state = 'unknown' AND NEW.state IN ('succeeded','failed'))
    ) THEN RAISE EXCEPTION 'invalid execution state transition'; END IF;
    IF OLD.first_unknown_at IS NOT NULL AND NEW.first_unknown_at IS DISTINCT FROM OLD.first_unknown_at THEN
        RAISE EXCEPTION 'uncertainty history is immutable';
    END IF;
    IF NEW.state = 'unknown' AND NEW.first_unknown_at IS NULL THEN
        NEW.first_unknown_at := now();
    END IF;
    IF OLD.state = 'unknown' AND NEW.state IN ('succeeded','failed')
       AND (NEW.resolved_at IS NULL OR NEW.outcome_evidence NOT IN ('operator_commit','operator_rollback')) THEN
        RAISE EXCEPTION 'unknown execution requires operator evidence';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_attempt_snapshot BEFORE INSERT OR UPDATE ON databrain.migration_attempts
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_attempt();

-- Role deployment disiapkan terpisah melalui dev/setup.sql.
REVOKE ALL ON SCHEMA databrain FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA databrain FROM PUBLIC;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA databrain FROM PUBLIC;
GRANT USAGE ON SCHEMA databrain TO databrain_runtime;
GRANT SELECT, INSERT, UPDATE ON databrain.users, databrain.groups,
    databrain.database_targets, databrain.migration_drafts,
    databrain.migration_streams, databrain.migration_attempts TO databrain_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON databrain.memberships,
    databrain.database_grants, databrain.sessions, databrain.session_tokens,
    databrain.login_windows TO databrain_runtime;
GRANT SELECT, UPDATE ON databrain.security_guard TO databrain_runtime;
-- Hanya metadata golang-migrate berada di luar schema aplikasi.
GRANT USAGE ON SCHEMA public TO databrain_runtime;
GRANT SELECT ON public.schema_migrations TO databrain_runtime;
