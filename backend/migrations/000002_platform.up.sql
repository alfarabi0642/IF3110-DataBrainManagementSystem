-- Eksekusi M2: plan immutable, attempt worker append-only, dan receipt durable. Upgrade additive;
-- baris, snapshot, constraint, dan gate M1 pada migration_attempts/migration_streams tidak diubah.
CREATE TABLE databrain.migration_executions (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    actor_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    request_id uuid NOT NULL,
    definition_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    target_config_revision integer NOT NULL CHECK (target_config_revision > 0),
    direction text NOT NULL CHECK (direction IN ('apply','rollback','forward','resume','compensate')),
    mode text NOT NULL CHECK (mode IN ('normal','batch')),
    plan_hash text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    sql_snapshot text NOT NULL CHECK (octet_length(sql_snapshot) BETWEEN 1 AND 131072),
    down_snapshot text NOT NULL DEFAULT '' CHECK (octet_length(down_snapshot) <= 131072),
    expected_head_id uuid,
    source_execution_id uuid,
    phase text NOT NULL DEFAULT 'queued' CHECK (phase IN ('queued','running','blocked','finished')),
    outcome text CHECK (outcome IN ('succeeded','failed','partial','partially_reverted','unknown')),
    committed_rows bigint NOT NULL DEFAULT 0 CHECK (committed_rows >= 0),
    committed_batches bigint NOT NULL DEFAULT 0 CHECK (committed_batches >= 0),
    total_rows bigint CHECK (total_rows >= 0),
    has_partial_commits boolean NOT NULL DEFAULT false,
    reconciliation_pending boolean NOT NULL DEFAULT false,
    failure_code text,
    failure_message varchar(240),
    failure_sqlstate varchar(5),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (target_id, source_execution_id)
        REFERENCES databrain.migration_executions(target_id, id) ON DELETE RESTRICT,
    UNIQUE (target_id, id),
    CHECK ((phase IN ('queued','running') AND outcome IS NULL AND finished_at IS NULL)
        OR (phase = 'blocked' AND outcome IS NOT NULL
            AND outcome IN ('partial','partially_reverted','unknown') AND finished_at IS NULL)
        OR (phase = 'finished' AND outcome IS NOT NULL
            AND outcome IN ('succeeded','failed') AND finished_at IS NOT NULL))
);
CREATE INDEX executions_by_target ON databrain.migration_executions(target_id, created_at DESC, id);

CREATE FUNCTION databrain.protect_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.phase <> 'queued' OR NEW.outcome IS NOT NULL OR NEW.started_at IS NOT NULL
           OR NEW.finished_at IS NOT NULL OR NEW.committed_rows <> 0 OR NEW.committed_batches <> 0
           OR NEW.has_partial_commits OR NEW.reconciliation_pending
           OR NEW.failure_code IS NOT NULL OR NEW.failure_message IS NOT NULL OR NEW.failure_sqlstate IS NOT NULL THEN
            RAISE EXCEPTION 'execution must start queued and empty';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.id,NEW.target_id,NEW.actor_id,NEW.request_id,NEW.definition_id,NEW.revision,
           NEW.target_config_revision,NEW.direction,NEW.mode,NEW.plan_hash,NEW.sql_snapshot,
           NEW.down_snapshot,NEW.expected_head_id,NEW.source_execution_id,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.target_id,OLD.actor_id,OLD.request_id,OLD.definition_id,OLD.revision,
           OLD.target_config_revision,OLD.direction,OLD.mode,OLD.plan_hash,OLD.sql_snapshot,
           OLD.down_snapshot,OLD.expected_head_id,OLD.source_execution_id,OLD.created_at) THEN
        RAISE EXCEPTION 'execution plan is immutable';
    END IF;
    IF OLD.phase = 'finished' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'finished execution is immutable';
    END IF;
    IF NEW.committed_rows < OLD.committed_rows OR NEW.committed_batches < OLD.committed_batches THEN
        RAISE EXCEPTION 'committed counters cannot decrease';
    END IF;
    IF OLD.has_partial_commits AND NOT NEW.has_partial_commits THEN
        RAISE EXCEPTION 'partial commit evidence is immutable';
    END IF;
    IF NEW.phase = 'running' AND NEW.started_at IS NULL THEN
        NEW.started_at := clock_timestamp();
    END IF;
    IF NEW.phase = 'finished' AND NEW.finished_at IS NULL THEN
        NEW.finished_at := clock_timestamp();
    END IF;
    IF NEW.phase IN ('queued','running') AND (NEW.outcome IS NOT NULL OR NEW.finished_at IS NOT NULL) THEN
        RAISE EXCEPTION 'execution phase and outcome are inconsistent';
    END IF;
    IF NEW.phase = 'blocked' AND (NEW.outcome IS NULL
       OR NEW.outcome NOT IN ('partial','partially_reverted','unknown') OR NEW.finished_at IS NOT NULL) THEN
        RAISE EXCEPTION 'blocked execution requires an incomplete outcome';
    END IF;
    IF NEW.phase = 'finished' AND (NEW.outcome IS NULL
       OR NEW.outcome NOT IN ('succeeded','failed') OR NEW.finished_at IS NULL) THEN
        RAISE EXCEPTION 'finished execution requires a terminal outcome';
    END IF;
    IF NEW.phase <> OLD.phase AND NOT (
        (OLD.phase = 'queued' AND NEW.phase IN ('running','blocked','finished')) OR
        (OLD.phase = 'running' AND NEW.phase IN ('blocked','finished')) OR
        (OLD.phase = 'blocked' AND NEW.phase IN ('running','finished'))
    ) THEN
        RAISE EXCEPTION 'invalid execution phase transition';
    END IF;
    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_execution_snapshot BEFORE INSERT OR UPDATE ON databrain.migration_executions
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_execution();

CREATE TABLE databrain.migration_worker_attempts (
    execution_id uuid NOT NULL,
    attempt_number integer NOT NULL CHECK (attempt_number > 0),
    job_id bigint NOT NULL CHECK (job_id > 0),
    state text NOT NULL DEFAULT 'running' CHECK (state IN ('running','succeeded','failed','unknown','partial')),
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    failure_code text,
    failure_message varchar(240),
    failure_sqlstate varchar(5),
    PRIMARY KEY (execution_id, attempt_number),
    FOREIGN KEY (execution_id) REFERENCES databrain.migration_executions(id) ON DELETE RESTRICT,
    UNIQUE (job_id, attempt_number),
    CHECK ((state = 'running' AND finished_at IS NULL) OR (state <> 'running' AND finished_at IS NOT NULL))
);
CREATE INDEX worker_attempts_by_job ON databrain.migration_worker_attempts(job_id);

CREATE FUNCTION databrain.protect_worker_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'running' THEN RAISE EXCEPTION 'worker attempt must start running'; END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.execution_id,NEW.attempt_number,NEW.job_id,NEW.started_at)
       IS DISTINCT FROM ROW(OLD.execution_id,OLD.attempt_number,OLD.job_id,OLD.started_at) THEN
        RAISE EXCEPTION 'worker attempt identity is immutable';
    END IF;
    IF OLD.state <> 'running' THEN
        RAISE EXCEPTION 'completed worker attempt is immutable';
    END IF;
    IF NEW.state = 'running' THEN
        RAISE EXCEPTION 'worker attempt update must record a terminal state';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_worker_attempt_snapshot BEFORE INSERT OR UPDATE ON databrain.migration_worker_attempts
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_worker_attempt();

CREATE TABLE databrain.execution_receipts (
    target_id uuid NOT NULL,
    actor_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    request_key uuid NOT NULL,
    intent_hash text NOT NULL CHECK (intent_hash ~ '^[0-9a-f]{64}$'),
    execution_id uuid NOT NULL,
    job_id bigint NOT NULL CHECK (job_id > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, actor_id, request_key),
    UNIQUE (execution_id),
    UNIQUE (job_id),
    FOREIGN KEY (target_id, execution_id) REFERENCES databrain.migration_executions(target_id, id) ON DELETE RESTRICT
);

CREATE FUNCTION databrain.protect_execution_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution receipt is immutable';
END;
$$;
CREATE TRIGGER protect_execution_receipt_snapshot BEFORE UPDATE OR DELETE ON databrain.execution_receipts
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_execution_receipt();

ALTER TABLE databrain.migration_streams ADD COLUMN active_execution_id uuid;
ALTER TABLE databrain.migration_streams ADD CONSTRAINT migration_streams_active_execution_fk
    FOREIGN KEY (target_id, active_execution_id)
    REFERENCES databrain.migration_executions(target_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE databrain.migration_streams ADD CONSTRAINT migration_streams_single_active_owner
    CHECK (active_attempt_id IS NULL OR active_execution_id IS NULL);

REVOKE EXECUTE ON FUNCTION databrain.protect_execution(),
    databrain.protect_worker_attempt(), databrain.protect_execution_receipt() FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON databrain.migration_executions,
    databrain.migration_worker_attempts TO databrain_runtime;
GRANT SELECT, INSERT ON databrain.execution_receipts TO databrain_runtime;

-- Immutable migration definitions, legacy import, and frozen draft identities.

ALTER TABLE databrain.migration_drafts
    ADD COLUMN down_sql text NOT NULL DEFAULT '',
    ADD COLUMN irreversible boolean NOT NULL DEFAULT false,
    ADD COLUMN irreversible_reason varchar(500),
    ADD CONSTRAINT migration_drafts_down_sql_size CHECK (octet_length(down_sql) <= 131072);

CREATE TABLE databrain.migration_definitions (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL,
    draft_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    up_sql text NOT NULL CHECK (octet_length(up_sql) BETWEEN 1 AND 131072),
    down_sql text NOT NULL DEFAULT '' CHECK (octet_length(down_sql) <= 131072),
    irreversible boolean NOT NULL,
    irreversible_reason varchar(500),
    up_sha256 bytea NOT NULL CHECK (octet_length(up_sha256) = 32),
    down_sha256 bytea CHECK (down_sha256 IS NULL OR octet_length(down_sha256) = 32),
    source text NOT NULL CHECK (source IN ('legacy_attempt', 'submission')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (target_id, id),
    UNIQUE (target_id, draft_id, revision),
    FOREIGN KEY (target_id, draft_id) REFERENCES databrain.migration_drafts(target_id, id) ON DELETE RESTRICT,
    CHECK (
        (irreversible AND down_sql = '' AND down_sha256 IS NULL AND irreversible_reason IS NOT NULL
            AND btrim(irreversible_reason) <> '')
        OR
        (NOT irreversible AND down_sql <> '' AND down_sha256 IS NOT NULL
            AND irreversible_reason IS NULL)
    )
);

-- Laporan rekonsiliasi untuk snapshot legacy yang bertentangan atau eksekusi v2 yang belum dapat
-- dipetakan ke definition. Tabel ini append-only dan hanya ditulis migrator.
CREATE TABLE databrain.migration_definition_issues (
    id bigserial PRIMARY KEY,
    target_id uuid NOT NULL,
    draft_id uuid,
    revision bigint,
    kind text NOT NULL CHECK (kind IN ('legacy_snapshot_mismatch', 'unmatched_execution')),
    detail text NOT NULL CHECK (octet_length(detail) BETWEEN 1 AND 1000),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Attempt yang konsisten menjadi satu definition legacy. Down tidak pernah ada di M1, jadi alasan
-- irreversible eksplisit dicatat.
INSERT INTO databrain.migration_definition_issues (target_id, draft_id, revision, kind, detail)
SELECT target_id, draft_id, draft_revision, 'legacy_snapshot_mismatch',
       'Repeated attempts for this draft revision have different SQL snapshots or hashes.'
FROM databrain.migration_attempts
GROUP BY target_id, draft_id, draft_revision
HAVING count(DISTINCT sql_snapshot) <> 1 OR count(DISTINCT sql_sha256) <> 1;

INSERT INTO databrain.migration_definitions
    (id, target_id, draft_id, revision, up_sql, down_sql, irreversible, irreversible_reason,
     up_sha256, down_sha256, source)
SELECT gen_random_uuid(), target_id, draft_id, draft_revision, min(sql_snapshot), '', true,
       'Legacy M1 revision has no down SQL.',
       decode(min(encode(sql_sha256, 'hex')), 'hex'), NULL, 'legacy_attempt'
FROM databrain.migration_attempts
GROUP BY target_id, draft_id, draft_revision
HAVING count(DISTINCT sql_snapshot) = 1 AND count(DISTINCT sql_sha256) = 1;

-- Matching current draft revisions get the same explicit legacy reason so the frozen editor does
-- not show an unexplained empty down. Newer saved draft revisions are left untouched.
UPDATE databrain.migration_drafts d
SET irreversible = true, irreversible_reason = 'Legacy M1 revision has no down SQL.', down_sql = ''
WHERE d.down_sql = '' AND NOT d.irreversible
  AND EXISTS (SELECT 1 FROM databrain.migration_attempts a
              WHERE a.target_id = d.target_id AND a.draft_id = d.id
                AND a.draft_revision = d.revision AND a.sql_snapshot = d.sql);

INSERT INTO databrain.migration_definition_issues (target_id, kind, detail)
SELECT DISTINCT e.target_id, 'unmatched_execution',
       'Execution ' || e.id || ' references definition ' || e.definition_id
FROM databrain.migration_executions e
WHERE NOT EXISTS (SELECT 1 FROM databrain.migration_definitions d WHERE d.id = e.definition_id);

CREATE FUNCTION databrain.protect_definition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'migration definition is immutable';
END;
$$;
CREATE TRIGGER protect_definition_snapshot BEFORE UPDATE OR DELETE ON databrain.migration_definitions
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_definition();

-- Sebuah draft yang sudah pernah direservasi/dieksekusi atau sudah disubmit tidak boleh berubah lagi.
-- Koreksi memakai identitas draft baru, sehingga snapshot lama tetap utuh.
CREATE FUNCTION databrain.protect_frozen_draft() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM databrain.migration_attempts
               WHERE target_id = OLD.target_id AND draft_id = OLD.id)
       OR EXISTS (SELECT 1 FROM databrain.migration_definitions
                  WHERE target_id = OLD.target_id AND draft_id = OLD.id)
    THEN
        RAISE EXCEPTION 'draft is frozen';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_frozen_draft_snapshot BEFORE UPDATE ON databrain.migration_drafts
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_frozen_draft();

CREATE INDEX migration_attempts_by_draft ON databrain.migration_attempts (target_id, draft_id);

REVOKE EXECUTE ON FUNCTION databrain.protect_definition(),
    databrain.protect_frozen_draft() FROM PUBLIC;
GRANT SELECT, INSERT ON databrain.migration_definitions TO databrain_runtime;
GRANT SELECT ON databrain.migration_definition_issues TO databrain_runtime;

-- Group leaders and target-scoped review-policy generation.

ALTER TABLE databrain.groups ADD COLUMN leader_id uuid;
ALTER TABLE databrain.groups ADD CONSTRAINT groups_leader_membership_fk
    FOREIGN KEY (id, leader_id) REFERENCES databrain.memberships(group_id, user_id) ON DELETE RESTRICT;
CREATE INDEX groups_by_leader ON databrain.groups (leader_id) WHERE leader_id IS NOT NULL;

CREATE TABLE databrain.migration_policy_generations (
    target_id uuid PRIMARY KEY REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO databrain.migration_policy_generations (target_id)
SELECT id FROM databrain.database_targets;

GRANT SELECT, INSERT, UPDATE ON databrain.migration_policy_generations TO databrain_runtime;

-- Immutable review requests, snapshotted required groups, and explicit leader decisions.

CREATE TABLE databrain.migration_requests (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    draft_id uuid NOT NULL,
    definition_id uuid NOT NULL,
    actor_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    actor_username varchar(32) NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    target_config_revision integer NOT NULL CHECK (target_config_revision > 0),
    direction text NOT NULL DEFAULT 'apply' CHECK (direction IN ('apply', 'rollback', 'forward')),
    mode text NOT NULL DEFAULT 'normal' CHECK (mode IN ('normal', 'batch')),
    source_execution_id uuid,
    expected_head_id uuid,
    head_generation bigint NOT NULL DEFAULT 1 CHECK (head_generation > 0),
    policy_generation bigint NOT NULL CHECK (policy_generation > 0),
    request_key uuid NOT NULL,
    state text NOT NULL DEFAULT 'pending_review'
        CHECK (state IN ('pending_review', 'approved', 'stale', 'consumed')),
    stale_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (target_id, id),
    UNIQUE (target_id, actor_id, request_key),
    FOREIGN KEY (target_id, draft_id) REFERENCES databrain.migration_drafts(target_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (target_id, definition_id) REFERENCES databrain.migration_definitions(target_id, id) ON DELETE RESTRICT,
    CHECK (direction = 'apply' OR source_execution_id IS NOT NULL)
);
CREATE INDEX migration_requests_by_target_time ON databrain.migration_requests (target_id, created_at DESC, id);

CREATE TABLE databrain.migration_request_groups (
    target_id uuid NOT NULL,
    request_id uuid NOT NULL,
    group_id uuid NOT NULL REFERENCES databrain.groups(id) ON DELETE RESTRICT,
    group_name varchar(64) NOT NULL,
    access_level text NOT NULL CHECK (access_level IN ('view_only', 'view_edit')),
    leader_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, request_id, group_id),
    FOREIGN KEY (target_id, request_id) REFERENCES databrain.migration_requests(target_id, id) ON DELETE RESTRICT
);

CREATE TABLE databrain.migration_request_decisions (
    target_id uuid NOT NULL,
    request_id uuid NOT NULL,
    group_id uuid NOT NULL,
    actor_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    leader_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, request_id, group_id),
    FOREIGN KEY (target_id, request_id, group_id)
        REFERENCES databrain.migration_request_groups(target_id, request_id, group_id) ON DELETE RESTRICT,
    CHECK (actor_id = leader_id)
);

CREATE FUNCTION databrain.protect_request_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'review request is immutable';
    END IF;
    IF ROW(NEW.id, NEW.target_id, NEW.draft_id, NEW.definition_id, NEW.actor_id, NEW.actor_username,
           NEW.revision, NEW.target_config_revision, NEW.direction, NEW.mode, NEW.source_execution_id,
           NEW.expected_head_id, NEW.head_generation, NEW.policy_generation, NEW.request_key, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.target_id, OLD.draft_id, OLD.definition_id, OLD.actor_id, OLD.actor_username,
           OLD.revision, OLD.target_config_revision, OLD.direction, OLD.mode, OLD.source_execution_id,
           OLD.expected_head_id, OLD.head_generation, OLD.policy_generation, OLD.request_key, OLD.created_at)
    THEN
        RAISE EXCEPTION 'review request content is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_request_snapshot BEFORE UPDATE OR DELETE ON databrain.migration_requests
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_request_snapshot();

CREATE FUNCTION databrain.protect_request_child() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'review request child is immutable';
END;
$$;
CREATE TRIGGER protect_request_groups_snapshot BEFORE UPDATE OR DELETE ON databrain.migration_request_groups
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_request_child();
CREATE TRIGGER protect_request_decisions_snapshot BEFORE UPDATE OR DELETE ON databrain.migration_request_decisions
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_request_child();

REVOKE EXECUTE ON FUNCTION databrain.protect_request_snapshot(),
    databrain.protect_request_child() FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON databrain.migration_requests TO databrain_runtime;
GRANT SELECT, INSERT ON databrain.migration_request_groups, databrain.migration_request_decisions TO databrain_runtime;

-- Bind an approved review request to exactly one execution/job.

ALTER TABLE databrain.migration_requests
    ADD COLUMN execution_id uuid,
    ADD COLUMN job_id bigint,
    ADD COLUMN consumed_request_key uuid,
    ADD COLUMN consumed_at timestamptz;

ALTER TABLE databrain.migration_requests ADD CONSTRAINT migration_requests_admission_consistent CHECK (
    (execution_id IS NULL AND job_id IS NULL AND consumed_request_key IS NULL AND consumed_at IS NULL
        AND state <> 'consumed')
    OR
    (execution_id IS NOT NULL AND job_id IS NOT NULL AND job_id > 0
        AND consumed_request_key IS NOT NULL AND consumed_at IS NOT NULL AND state = 'consumed')
);
ALTER TABLE databrain.migration_requests ADD CONSTRAINT migration_requests_execution_fk
    FOREIGN KEY (target_id, execution_id) REFERENCES databrain.migration_executions(target_id, id)
    DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX migration_requests_by_execution ON databrain.migration_requests (target_id, execution_id)
    WHERE execution_id IS NOT NULL;

CREATE FUNCTION databrain.protect_consumed_request() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'consumed' AND ROW(NEW.state, NEW.execution_id, NEW.job_id,
           NEW.consumed_request_key, NEW.consumed_at)
       IS DISTINCT FROM
       ROW(OLD.state, OLD.execution_id, OLD.job_id, OLD.consumed_request_key, OLD.consumed_at)
    THEN
        RAISE EXCEPTION 'consumed review request is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protect_consumed_request_snapshot BEFORE UPDATE ON databrain.migration_requests
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_consumed_request();

REVOKE EXECUTE ON FUNCTION databrain.protect_consumed_request() FROM PUBLIC;

-- Committed head, append-only events, and target-local redo stack.

CREATE TABLE databrain.migration_committed_events (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL,
    definition_id uuid NOT NULL,
    actor_id uuid NOT NULL REFERENCES databrain.users(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    direction text NOT NULL CHECK (direction IN ('apply', 'rollback', 'forward')),
    mode text NOT NULL CHECK (mode IN ('normal', 'batch')),
    plan_hash text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    commit_evidence text NOT NULL CHECK (octet_length(commit_evidence) BETWEEN 1 AND 200),
    parent_execution_id uuid,
    canceled_execution_id uuid,
    generation bigint NOT NULL CHECK (generation > 0),
    committed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (target_id, id),
    UNIQUE (target_id, execution_id),
    UNIQUE (target_id, generation),
    FOREIGN KEY (target_id, execution_id) REFERENCES databrain.migration_executions(target_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (target_id, definition_id) REFERENCES databrain.migration_definitions(target_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (target_id, parent_execution_id) REFERENCES databrain.migration_committed_events(target_id, execution_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (target_id, canceled_execution_id) REFERENCES databrain.migration_committed_events(target_id, execution_id) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((direction = 'rollback' AND canceled_execution_id IS NOT NULL)
        OR (direction <> 'rollback' AND canceled_execution_id IS NULL))
);
CREATE UNIQUE INDEX migration_committed_events_one_cancellation ON databrain.migration_committed_events (target_id, canceled_execution_id)
    WHERE canceled_execution_id IS NOT NULL;
CREATE INDEX migration_committed_events_by_target_time ON databrain.migration_committed_events (target_id, committed_at, id);

CREATE TABLE databrain.migration_target_heads (
    target_id uuid PRIMARY KEY REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    active_execution_id uuid,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    initialized boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (target_id, active_execution_id) REFERENCES databrain.migration_committed_events(target_id, execution_id) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE databrain.migration_redo_entries (
    target_id uuid NOT NULL REFERENCES databrain.database_targets(id) ON DELETE RESTRICT,
    position bigint NOT NULL CHECK (position > 0),
    source_execution_id uuid NOT NULL,
    definition_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, position),
    UNIQUE (target_id, source_execution_id),
    FOREIGN KEY (target_id, source_execution_id) REFERENCES databrain.migration_committed_events(target_id, execution_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (target_id, definition_id) REFERENCES databrain.migration_definitions(target_id, id) DEFERRABLE INITIALLY DEFERRED
);

-- Existing empty targets start initialized with no active head. Targets with any committed/running
-- evidence stay uninitialized until the cutover imports verified positions.
INSERT INTO databrain.migration_target_heads (target_id, generation, initialized)
SELECT t.id, 1,
       NOT EXISTS (SELECT 1 FROM databrain.migration_attempts a WHERE a.target_id=t.id AND a.state='succeeded')
       AND NOT EXISTS (SELECT 1 FROM databrain.migration_executions e WHERE e.target_id=t.id AND e.phase IN ('blocked','finished'))
FROM databrain.database_targets t;

CREATE FUNCTION databrain.initialize_target_head() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO databrain.migration_target_heads (target_id, generation, initialized)
    VALUES (NEW.id, 1, true) ON CONFLICT (target_id) DO NOTHING;
    RETURN NEW;
END;
$$;
CREATE TRIGGER initialize_target_head AFTER INSERT ON databrain.database_targets
    FOR EACH ROW EXECUTE FUNCTION databrain.initialize_target_head();

CREATE FUNCTION databrain.protect_committed_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'committed event is immutable';
END;
$$;
CREATE TRIGGER protect_committed_event BEFORE UPDATE OR DELETE ON databrain.migration_committed_events
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_committed_event();

REVOKE EXECUTE ON FUNCTION databrain.initialize_target_head(),
    databrain.protect_committed_event() FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON databrain.migration_target_heads TO databrain_runtime;
GRANT SELECT, INSERT ON databrain.migration_committed_events TO databrain_runtime;
GRANT SELECT, INSERT, DELETE ON databrain.migration_redo_entries TO databrain_runtime;

-- Bounded batch configuration on requests/executions, recovery directions, and
-- append-only committed batch projection (without private before-images).

ALTER TABLE databrain.migration_requests
    ADD COLUMN batch_config jsonb,
    DROP CONSTRAINT migration_requests_direction_check,
    ADD CONSTRAINT migration_requests_direction_check
        CHECK (direction IN ('apply', 'rollback', 'forward', 'resume', 'compensate')),
    ADD CONSTRAINT migration_requests_batch_mode_consistent
        CHECK ((mode = 'normal' AND batch_config IS NULL)
            OR (mode = 'batch' AND batch_config IS NOT NULL));

ALTER TABLE databrain.migration_executions
    ADD COLUMN batch_config jsonb,
    ADD CONSTRAINT migration_executions_batch_mode_consistent
        CHECK ((mode = 'normal' AND batch_config IS NULL)
            OR (mode = 'batch' AND batch_config IS NOT NULL));

CREATE TABLE databrain.migration_execution_batches (
    target_id uuid NOT NULL,
    execution_id uuid NOT NULL,
    batch_ordinal integer NOT NULL CHECK (batch_ordinal > 0),
    direction text NOT NULL CHECK (direction IN ('apply', 'resume', 'compensate')),
    action_execution_id uuid NOT NULL,
    rows_affected bigint NOT NULL CHECK (rows_affected >= 0),
    cursor_end text,
    committed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, execution_id, batch_ordinal, direction),
    FOREIGN KEY (target_id, execution_id)
        REFERENCES databrain.migration_executions(target_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (target_id, action_execution_id)
        REFERENCES databrain.migration_executions(target_id, id) ON DELETE RESTRICT
);
CREATE INDEX execution_batches_by_execution
    ON databrain.migration_execution_batches (target_id, execution_id, batch_ordinal, direction);

CREATE FUNCTION databrain.protect_execution_batch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'committed batch record is immutable';
END;
$$;
CREATE TRIGGER protect_execution_batch_snapshot BEFORE UPDATE OR DELETE ON databrain.migration_execution_batches
    FOR EACH ROW EXECUTE FUNCTION databrain.protect_execution_batch();

REVOKE EXECUTE ON FUNCTION databrain.protect_execution_batch() FROM PUBLIC;
GRANT SELECT, INSERT ON databrain.migration_execution_batches TO databrain_runtime;
