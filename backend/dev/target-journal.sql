-- DataBrain M2 target journal provisioning
-- Run as an administrative/database-owner role on each target database managed by DataBrain
-- (e.g. psql -X -v ON_ERROR_STOP=1 -U databrain_fixture_owner -d <target_db> -f backend/dev/target-journal.sql).
--
-- Prerequisites:
--   1. The target database MUST NOT be owned by databrain_target (a PostgreSQL database owner can
--      DROP SCHEMA databrain_runtime CASCADE even without owning the schema). If an existing M1
--      target database is owned by databrain_target, transfer ownership first:
--        ALTER DATABASE <target_db> OWNER TO <admin_owner>;
--        GRANT CONNECT ON DATABASE <target_db> TO databrain_target;
--        GRANT USAGE, CREATE ON SCHEMA public TO databrain_target;
--   2. This script is idempotent and fails closed if privilege separation cannot be established.

DO $$
DECLARE
    journal_owner_unsafe boolean;
    target_unsafe boolean;
    target_owns_db boolean;
    target_in_journal_owner boolean;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'databrain_journal_owner') THEN
        CREATE ROLE databrain_journal_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
            NOREPLICATION NOBYPASSRLS;
    END IF;

    SELECT rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
      INTO journal_owner_unsafe
      FROM pg_catalog.pg_roles
     WHERE rolname = 'databrain_journal_owner';
    IF journal_owner_unsafe THEN
        RAISE EXCEPTION 'databrain_journal_owner must be a restricted NOLOGIN role';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'databrain_target') THEN
        RAISE EXCEPTION 'target role databrain_target does not exist';
    END IF;

    SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
      INTO target_unsafe
      FROM pg_catalog.pg_roles
     WHERE rolname = 'databrain_target';
    IF target_unsafe THEN
        RAISE EXCEPTION 'databrain_target has unsafe cluster privileges';
    END IF;

    SELECT pg_catalog.pg_has_role('databrain_target', d.datdba, 'MEMBER')
      INTO target_owns_db
      FROM pg_catalog.pg_database d
     WHERE d.datname = pg_catalog.current_database();
    IF target_owns_db THEN
        RAISE EXCEPTION 'target database must not be owned by databrain_target; transfer database ownership before provisioning databrain_runtime';
    END IF;

    SELECT pg_catalog.pg_has_role('databrain_target', 'databrain_journal_owner', 'MEMBER')
      INTO target_in_journal_owner;
    IF target_in_journal_owner THEN
        RAISE EXCEPTION 'databrain_target must not be a member of databrain_journal_owner';
    END IF;
END;
$$;

CREATE SCHEMA IF NOT EXISTS databrain_runtime AUTHORIZATION databrain_journal_owner;
ALTER SCHEMA databrain_runtime OWNER TO databrain_journal_owner;
REVOKE ALL ON SCHEMA databrain_runtime FROM PUBLIC;
REVOKE ALL ON SCHEMA databrain_runtime FROM databrain_target;
GRANT USAGE ON SCHEMA databrain_runtime TO databrain_target;

CREATE TABLE IF NOT EXISTS databrain_runtime.journal_meta (
    id integer PRIMARY KEY CHECK (id = 1),
    schema_version integer NOT NULL CHECK (schema_version = 1),
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE databrain_runtime.journal_meta OWNER TO databrain_journal_owner;
INSERT INTO databrain_runtime.journal_meta (id, schema_version)
VALUES (1, 1) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS databrain_runtime.unresolved_owner (
    id integer PRIMARY KEY CHECK (id = 1),
    root_execution_id uuid NOT NULL,
    action_execution_id uuid NOT NULL,
    plan_hash text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    owner_secret_hash bytea NOT NULL CHECK (octet_length(owner_secret_hash) = 32),
    acquired_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE databrain_runtime.unresolved_owner OWNER TO databrain_journal_owner;

CREATE TABLE IF NOT EXISTS databrain_runtime.tx_nonces (
    xid xid8 PRIMARY KEY,
    root_execution_id uuid NOT NULL,
    action_execution_id uuid NOT NULL,
    plan_hash text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    nonce_hash bytea NOT NULL CHECK (octet_length(nonce_hash) = 32),
    consumed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE databrain_runtime.tx_nonces OWNER TO databrain_journal_owner;

CREATE TABLE IF NOT EXISTS databrain_runtime.execution_commits (
    root_execution_id uuid NOT NULL,
    batch_ordinal integer NOT NULL CHECK (batch_ordinal >= 0),
    direction text NOT NULL CHECK (direction IN ('apply', 'rollback', 'forward', 'resume', 'compensate')),
    action_execution_id uuid NOT NULL,
    plan_hash text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    rows_affected bigint NOT NULL DEFAULT 0 CHECK (rows_affected >= 0),
    cursor_end text,
    committed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (root_execution_id, batch_ordinal, direction)
);
ALTER TABLE databrain_runtime.execution_commits OWNER TO databrain_journal_owner;

CREATE TABLE IF NOT EXISTS databrain_runtime.batch_scope (
    root_execution_id uuid NOT NULL,
    batch_ordinal integer NOT NULL CHECK (batch_ordinal > 0),
    row_key bigint NOT NULL,
    before_value bigint NOT NULL,
    after_value bigint NOT NULL,
    PRIMARY KEY (root_execution_id, batch_ordinal, row_key)
);
ALTER TABLE databrain_runtime.batch_scope OWNER TO databrain_journal_owner;

REVOKE ALL ON TABLE
    databrain_runtime.journal_meta,
    databrain_runtime.unresolved_owner,
    databrain_runtime.tx_nonces,
    databrain_runtime.execution_commits,
    databrain_runtime.batch_scope
FROM PUBLIC, databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime._protect_immutable() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, databrain_runtime
AS $$
BEGIN
    RAISE EXCEPTION 'databrain_runtime evidence is immutable';
END;
$$;
ALTER FUNCTION databrain_runtime._protect_immutable() OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime._protect_immutable() FROM PUBLIC, databrain_target;

DROP TRIGGER IF EXISTS protect_journal_meta ON databrain_runtime.journal_meta;
CREATE TRIGGER protect_journal_meta
    BEFORE UPDATE OR DELETE ON databrain_runtime.journal_meta
    FOR EACH ROW EXECUTE FUNCTION databrain_runtime._protect_immutable();

DROP TRIGGER IF EXISTS protect_execution_commits ON databrain_runtime.execution_commits;
CREATE TRIGGER protect_execution_commits
    BEFORE UPDATE OR DELETE ON databrain_runtime.execution_commits
    FOR EACH ROW EXECUTE FUNCTION databrain_runtime._protect_immutable();

CREATE OR REPLACE FUNCTION databrain_runtime._require_target_lock() RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, databrain_runtime
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_catalog.pg_locks l
          JOIN pg_catalog.pg_database d ON d.oid = l.database
         WHERE l.pid = pg_catalog.pg_backend_pid()
           AND d.datname = pg_catalog.current_database()
           AND l.locktype = 'advisory'
           AND l.classid = 3110
           AND l.objid = 2
           AND l.objsubid = 2
           AND l.granted
    ) THEN
        RAISE EXCEPTION 'target ownership advisory lock (3110,2) is required';
    END IF;
END;
$$;
ALTER FUNCTION databrain_runtime._require_target_lock() OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime._require_target_lock() FROM PUBLIC, databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.read_meta()
RETURNS TABLE (schema_version integer, created_at timestamptz)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
    SELECT m.schema_version, m.created_at
      FROM databrain_runtime.journal_meta m
     WHERE m.id = 1;
$$;
ALTER FUNCTION databrain_runtime.read_meta() OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.read_meta() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.read_meta() TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.read_owner()
RETURNS TABLE (
    root_execution_id uuid,
    action_execution_id uuid,
    plan_hash text,
    acquired_at timestamptz,
    updated_at timestamptz
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
    SELECT o.root_execution_id, o.action_execution_id, o.plan_hash, o.acquired_at, o.updated_at
      FROM databrain_runtime.unresolved_owner o
     WHERE o.id = 1;
$$;
ALTER FUNCTION databrain_runtime.read_owner() OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.read_owner() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.read_owner() TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.read_commits(p_root_execution_id uuid)
RETURNS TABLE (
    root_execution_id uuid,
    batch_ordinal integer,
    direction text,
    action_execution_id uuid,
    plan_hash text,
    rows_affected bigint,
    cursor_end text,
    committed_at timestamptz
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
    SELECT c.root_execution_id, c.batch_ordinal, c.direction, c.action_execution_id,
           c.plan_hash, c.rows_affected, c.cursor_end, c.committed_at
      FROM databrain_runtime.execution_commits c
     WHERE c.root_execution_id = p_root_execution_id
     ORDER BY c.batch_ordinal, c.direction;
$$;
ALTER FUNCTION databrain_runtime.read_commits(uuid) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.read_commits(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.read_commits(uuid) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.acquire_owner(
    p_root_execution_id uuid,
    p_action_execution_id uuid,
    p_plan_hash text,
    p_owner_secret_hash bytea
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
DECLARE
    existing databrain_runtime.unresolved_owner%ROWTYPE;
BEGIN
    PERFORM databrain_runtime._require_target_lock();
    IF p_root_execution_id IS NULL OR p_action_execution_id IS NULL
       OR p_plan_hash IS NULL OR p_plan_hash !~ '^[0-9a-f]{64}$'
       OR p_owner_secret_hash IS NULL OR pg_catalog.octet_length(p_owner_secret_hash) <> 32
    THEN
        RAISE EXCEPTION 'invalid unresolved owner arguments';
    END IF;

    SELECT * INTO existing FROM databrain_runtime.unresolved_owner WHERE id = 1 FOR UPDATE;
    IF FOUND THEN
        IF existing.root_execution_id = p_root_execution_id
           AND existing.action_execution_id = p_action_execution_id
           AND existing.plan_hash = p_plan_hash
           AND existing.owner_secret_hash = p_owner_secret_hash
        THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'target journal owned by another unresolved execution';
    END IF;

    INSERT INTO databrain_runtime.unresolved_owner
        (id, root_execution_id, action_execution_id, plan_hash, owner_secret_hash)
    VALUES
        (1, p_root_execution_id, p_action_execution_id, p_plan_hash, p_owner_secret_hash);
END;
$$;
ALTER FUNCTION databrain_runtime.acquire_owner(uuid, uuid, text, bytea) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.acquire_owner(uuid, uuid, text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.acquire_owner(uuid, uuid, text, bytea) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.transfer_owner(
    p_root_execution_id uuid,
    p_new_action_execution_id uuid,
    p_new_plan_hash text,
    p_owner_secret bytea
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
DECLARE
    existing databrain_runtime.unresolved_owner%ROWTYPE;
BEGIN
    PERFORM databrain_runtime._require_target_lock();
    IF p_root_execution_id IS NULL OR p_new_action_execution_id IS NULL
       OR p_new_plan_hash IS NULL OR p_new_plan_hash !~ '^[0-9a-f]{64}$'
       OR p_owner_secret IS NULL OR pg_catalog.octet_length(p_owner_secret) <> 32
    THEN
        RAISE EXCEPTION 'invalid transfer owner arguments';
    END IF;

    SELECT * INTO existing FROM databrain_runtime.unresolved_owner WHERE id = 1 FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'target journal has no unresolved owner to transfer';
    END IF;
    IF existing.root_execution_id <> p_root_execution_id
       OR existing.owner_secret_hash <> pg_catalog.sha256(p_owner_secret)
    THEN
        RAISE EXCEPTION 'target journal owner authorization failed';
    END IF;

    UPDATE databrain_runtime.unresolved_owner
       SET action_execution_id = p_new_action_execution_id,
           plan_hash = p_new_plan_hash,
           updated_at = now()
     WHERE id = 1;
END;
$$;
ALTER FUNCTION databrain_runtime.transfer_owner(uuid, uuid, text, bytea) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.transfer_owner(uuid, uuid, text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.transfer_owner(uuid, uuid, text, bytea) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.clear_owner(
    p_root_execution_id uuid,
    p_owner_secret bytea
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
DECLARE
    existing databrain_runtime.unresolved_owner%ROWTYPE;
BEGIN
    PERFORM databrain_runtime._require_target_lock();
    IF p_root_execution_id IS NULL
       OR p_owner_secret IS NULL OR pg_catalog.octet_length(p_owner_secret) <> 32
    THEN
        RAISE EXCEPTION 'invalid clear owner arguments';
    END IF;

    SELECT * INTO existing FROM databrain_runtime.unresolved_owner WHERE id = 1 FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF existing.root_execution_id <> p_root_execution_id
       OR existing.owner_secret_hash <> pg_catalog.sha256(p_owner_secret)
    THEN
        RAISE EXCEPTION 'target journal owner authorization failed';
    END IF;

    DELETE FROM databrain_runtime.unresolved_owner WHERE id = 1;
END;
$$;
ALTER FUNCTION databrain_runtime.clear_owner(uuid, bytea) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.clear_owner(uuid, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.clear_owner(uuid, bytea) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.arm_tx(
    p_root_execution_id uuid,
    p_action_execution_id uuid,
    p_plan_hash text,
    p_nonce_hash bytea
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
DECLARE
    existing databrain_runtime.unresolved_owner%ROWTYPE;
BEGIN
    PERFORM databrain_runtime._require_target_lock();
    IF p_root_execution_id IS NULL OR p_action_execution_id IS NULL
       OR p_plan_hash IS NULL OR p_plan_hash !~ '^[0-9a-f]{64}$'
       OR p_nonce_hash IS NULL OR pg_catalog.octet_length(p_nonce_hash) <> 32
    THEN
        RAISE EXCEPTION 'invalid transaction nonce arguments';
    END IF;

    SELECT * INTO existing FROM databrain_runtime.unresolved_owner WHERE id = 1;
    IF NOT FOUND
       OR existing.root_execution_id <> p_root_execution_id
       OR existing.action_execution_id <> p_action_execution_id
       OR existing.plan_hash <> p_plan_hash
    THEN
        RAISE EXCEPTION 'target journal owner mismatch before transaction arm';
    END IF;

    DELETE FROM databrain_runtime.tx_nonces
     WHERE xid < pg_catalog.pg_snapshot_xmin(pg_catalog.pg_current_snapshot());

    INSERT INTO databrain_runtime.tx_nonces
        (xid, root_execution_id, action_execution_id, plan_hash, nonce_hash, consumed)
    VALUES
        (pg_catalog.pg_current_xact_id(), p_root_execution_id, p_action_execution_id, p_plan_hash, p_nonce_hash, false);
END;
$$;
ALTER FUNCTION databrain_runtime.arm_tx(uuid, uuid, text, bytea) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.arm_tx(uuid, uuid, text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.arm_tx(uuid, uuid, text, bytea) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.record_commit(
    p_root_execution_id uuid,
    p_action_execution_id uuid,
    p_plan_hash text,
    p_batch_ordinal integer,
    p_direction text,
    p_rows_affected bigint,
    p_cursor_end text,
    p_nonce bytea
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
DECLARE
    guard databrain_runtime.tx_nonces%ROWTYPE;
BEGIN
    PERFORM databrain_runtime._require_target_lock();
    IF p_root_execution_id IS NULL OR p_action_execution_id IS NULL
       OR p_plan_hash IS NULL OR p_plan_hash !~ '^[0-9a-f]{64}$'
       OR p_batch_ordinal IS NULL OR p_batch_ordinal < 0
       OR p_direction IS NULL OR p_direction NOT IN ('apply', 'rollback', 'forward', 'resume', 'compensate')
       OR p_rows_affected IS NULL OR p_rows_affected < 0
       OR p_nonce IS NULL OR pg_catalog.octet_length(p_nonce) <> 32
    THEN
        RAISE EXCEPTION 'invalid commit evidence arguments';
    END IF;

    SELECT * INTO guard
      FROM databrain_runtime.tx_nonces
     WHERE xid = pg_catalog.pg_current_xact_id()
       FOR UPDATE;
    IF NOT FOUND
       OR guard.consumed
       OR guard.root_execution_id <> p_root_execution_id
       OR guard.action_execution_id <> p_action_execution_id
       OR guard.plan_hash <> p_plan_hash
       OR guard.nonce_hash <> pg_catalog.sha256(p_nonce)
    THEN
        RAISE EXCEPTION 'invalid or missing transaction commit nonce';
    END IF;

    UPDATE databrain_runtime.tx_nonces
       SET consumed = true
     WHERE xid = guard.xid;

    INSERT INTO databrain_runtime.execution_commits
        (root_execution_id, batch_ordinal, direction, action_execution_id, plan_hash, rows_affected, cursor_end)
    VALUES
        (p_root_execution_id, p_batch_ordinal, p_direction, p_action_execution_id, p_plan_hash, p_rows_affected, p_cursor_end);
END;
$$;
ALTER FUNCTION databrain_runtime.record_commit(uuid, uuid, text, integer, text, bigint, text, bytea) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.record_commit(uuid, uuid, text, integer, text, bigint, text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.record_commit(uuid, uuid, text, integer, text, bigint, text, bytea) TO databrain_target;

DROP TRIGGER IF EXISTS protect_batch_scope ON databrain_runtime.batch_scope;
CREATE TRIGGER protect_batch_scope
    BEFORE UPDATE OR DELETE ON databrain_runtime.batch_scope
    FOR EACH ROW EXECUTE FUNCTION databrain_runtime._protect_immutable();

CREATE OR REPLACE FUNCTION databrain_runtime.record_batch_scope(
    p_root_execution_id uuid,
    p_action_execution_id uuid,
    p_plan_hash text,
    p_batch_ordinal integer,
    p_keys bigint[],
    p_before bigint[],
    p_after bigint[]
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
DECLARE
    existing databrain_runtime.unresolved_owner%ROWTYPE;
BEGIN
    PERFORM databrain_runtime._require_target_lock();
    IF p_root_execution_id IS NULL OR p_action_execution_id IS NULL
       OR p_plan_hash IS NULL OR p_plan_hash !~ '^[0-9a-f]{64}$'
       OR p_batch_ordinal IS NULL OR p_batch_ordinal < 1
       OR p_keys IS NULL OR p_before IS NULL OR p_after IS NULL
       OR pg_catalog.array_length(p_keys, 1) IS NULL
       OR pg_catalog.array_length(p_keys, 1) <> pg_catalog.array_length(p_before, 1)
       OR pg_catalog.array_length(p_keys, 1) <> pg_catalog.array_length(p_after, 1)
    THEN
        RAISE EXCEPTION 'invalid batch scope arguments';
    END IF;

    SELECT * INTO existing FROM databrain_runtime.unresolved_owner WHERE id = 1;
    IF NOT FOUND
       OR existing.root_execution_id <> p_root_execution_id
       OR existing.action_execution_id <> p_action_execution_id
       OR existing.plan_hash <> p_plan_hash
    THEN
        RAISE EXCEPTION 'target journal owner mismatch before batch scope capture';
    END IF;

    INSERT INTO databrain_runtime.batch_scope
        (root_execution_id, batch_ordinal, row_key, before_value, after_value)
    SELECT p_root_execution_id, p_batch_ordinal, u.k, u.b, u.a
      FROM unnest(p_keys, p_before, p_after) AS u(k, b, a)
    ON CONFLICT (root_execution_id, batch_ordinal, row_key) DO NOTHING;
END;
$$;
ALTER FUNCTION databrain_runtime.record_batch_scope(uuid, uuid, text, integer, bigint[], bigint[], bigint[]) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.record_batch_scope(uuid, uuid, text, integer, bigint[], bigint[], bigint[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.record_batch_scope(uuid, uuid, text, integer, bigint[], bigint[], bigint[]) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.read_batch_scope_meta(p_root_execution_id uuid)
RETURNS TABLE (total_rows bigint, total_batches integer)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
    SELECT count(*)::bigint, coalesce(max(s.batch_ordinal), 0)::integer
      FROM databrain_runtime.batch_scope s
     WHERE s.root_execution_id = p_root_execution_id;
$$;
ALTER FUNCTION databrain_runtime.read_batch_scope_meta(uuid) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.read_batch_scope_meta(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.read_batch_scope_meta(uuid) TO databrain_target;

CREATE OR REPLACE FUNCTION databrain_runtime.read_batch_scope(p_root_execution_id uuid, p_batch_ordinal integer)
RETURNS TABLE (row_key bigint, before_value bigint, after_value bigint)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, databrain_runtime
AS $$
    SELECT s.row_key, s.before_value, s.after_value
      FROM databrain_runtime.batch_scope s
     WHERE s.root_execution_id = p_root_execution_id
       AND s.batch_ordinal = p_batch_ordinal
     ORDER BY s.row_key;
$$;
ALTER FUNCTION databrain_runtime.read_batch_scope(uuid, integer) OWNER TO databrain_journal_owner;
REVOKE ALL ON FUNCTION databrain_runtime.read_batch_scope(uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION databrain_runtime.read_batch_scope(uuid, integer) TO databrain_target;
