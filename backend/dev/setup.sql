-- Jalankan sekali sebagai databrain_fixture_owner pada cluster disposable baru. Role/database yang sudah ada menyebabkan error; script tidak mereset data.
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

-- Jangan mencatat SQL berisi credential, termasuk statement gagal.
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

-- Owner dapat membuat objek. Runtime hanya mendapat CONNECT; hak objek databrain diberikan migration aplikasi. Tidak ada membership, akun, tabel aplikasi, atau data demo yang dibuat di sini.
