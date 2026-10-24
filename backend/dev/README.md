# Local PostgreSQL fixture

Run these commands from the repository root. The fixture uses only
`127.0.0.1:55432` and stores its data under the ignored `.tmp/` directory.

```bash
export DATABRAIN_FIXTURE_DATA_DIR="$PWD/.tmp/postgres/data"
export DATABRAIN_FIXTURE_PASSWORD='choose-a-local-fixture-password'
export DATABRAIN_OWNER_PASSWORD="$DATABRAIN_FIXTURE_PASSWORD"
export DATABRAIN_MIGRATOR_PASSWORD='choose-a-local-migrator-password'
export DATABRAIN_RUNTIME_PASSWORD='choose-a-local-runtime-password'
export DATABRAIN_TARGET_PASSWORD='choose-a-local-target-password'
mkdir -p "$DATABRAIN_FIXTURE_DATA_DIR"
docker compose -f infra/compose.yaml up -d --wait
PGPASSWORD="$DATABRAIN_FIXTURE_PASSWORD" psql -h 127.0.0.1 -p 55432 \
  -U databrain_fixture_owner -d postgres -f backend/dev/setup.sql
```

`setup.sql` is intentionally one-shot: it refuses an unexpected cluster and
fails if its roles or databases already exist. For an existing initialized
fixture, start Compose without running the script again.

Configure and start the application from `backend/`:

```bash
export APP_ENV=development
export APP_ORIGIN=http://localhost:8080
export HTTP_ADDR=127.0.0.1:8080
export PLATFORM_DATABASE_URL="postgres://databrain_runtime:$DATABRAIN_RUNTIME_PASSWORD@127.0.0.1:55432/databrain_dev?sslmode=disable"
export MIGRATOR_DATABASE_URL="postgres://databrain_migrator:$DATABRAIN_MIGRATOR_PASSWORD@127.0.0.1:55432/databrain_dev?sslmode=disable"
export APP_SECRET_KEY="$(openssl rand -base64 32)"
export TARGET_ALLOWED_HOSTS=127.0.0.1,localhost

go run ./cmd/manage migrate up
printf '%s' 'choose-an-admin-password' > ../.tmp/admin-password
go run ./cmd/manage bootstrap-admin --username admin --display-name Administrator \
  --password-file ../.tmp/admin-password
go run ./cmd/api
```

The application does not load `.env` automatically. Keep `APP_SECRET_KEY`
stable across restarts or existing sessions and stored target credentials will
be unreadable.

To run every PostgreSQL integration test:

```bash
export DATABRAIN_TEST_ADMIN_URL="postgres://databrain_fixture_owner:$DATABRAIN_FIXTURE_PASSWORD@127.0.0.1:55432/postgres?sslmode=disable"
go test -race -count=1 ./...
```

Stop the fixture with `docker compose -f infra/compose.yaml down`. Delete
`.tmp/postgres/data` only when intentionally discarding this local fixture.
