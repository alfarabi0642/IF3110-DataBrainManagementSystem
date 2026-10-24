-- Run once as databrain_fixture_owner in the new local disposable cluster.
-- Existing roles/databases cause an error; this script never resets them.
\set ON_ERROR_STOP on
\set ECHO none
\getenv migrator_password DATABRAIN_MIGRATOR_PASSWORD
\getenv runtime_password DATABRAIN_RUNTIME_PASSWORD
\getenv target_password DATABRAIN_TARGET_PASSWORD
\getenv fixture_data_dir DATABRAIN_FIXTURE_DATA_DIR

SELECT set_config('databrain.fixture_data_dir', :'fixture_data_dir', false);

DO $$
BEGIN
    IF current_user <> 'databrain_fixture_owner'
       OR current_database() <> 'postgres'
       OR current_setting('data_directory') <> current_setting('databrain.fixture_data_dir') THEN
        RAISE EXCEPTION 'Use only the verified local disposable cluster';
    END IF;
END;
$$;

-- Do not record SQL containing credentials, including statements that fail.
SET log_statement = 'none';
SET log_min_error_statement = 'panic';
SET password_encryption = 'scram-sha-256';

CREATE ROLE databrain_migrator LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
    NOREPLICATION NOBYPASSRLS PASSWORD :'migrator_password';
CREATE ROLE databrain_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
    NOREPLICATION NOBYPASSRLS PASSWORD :'runtime_password';
CREATE ROLE databrain_target LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
    NOREPLICATION NOBYPASSRLS PASSWORD :'target_password';

CREATE DATABASE databrain_dev OWNER databrain_migrator;
CREATE DATABASE school_demo OWNER databrain_target;
CREATE DATABASE inventory_demo OWNER databrain_target;

REVOKE ALL ON DATABASE databrain_dev, school_demo, inventory_demo FROM PUBLIC;
REVOKE ALL ON DATABASE postgres, template1 FROM PUBLIC;
GRANT CONNECT ON DATABASE databrain_dev TO databrain_runtime;

\connect databrain_dev
REVOKE ALL ON SCHEMA public FROM PUBLIC;
\connect school_demo
REVOKE ALL ON SCHEMA public FROM PUBLIC;
\connect inventory_demo
REVOKE ALL ON SCHEMA public FROM PUBLIC;

-- The database owners can create their schemas/objects. Runtime receives only
-- CONNECT here; grants on databrain objects belong to the application migration.
-- No memberships, application accounts, application tables or demo data are seeded.
