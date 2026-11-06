#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"
if [ "${DATABRAIN_DATABASE_MODE:-docker}" = existing ]; then
  : "${APP_SECRET_KEY:?Mode existing memerlukan APP_SECRET_KEY yang sama dengan instalasi sebelumnya}"
fi

export DATABRAIN_FIXTURE_PASSWORD="${DATABRAIN_FIXTURE_PASSWORD:-localsecretpassword}"
export DATABRAIN_MIGRATOR_PASSWORD="${DATABRAIN_MIGRATOR_PASSWORD:-migratorpassword}"
export DATABRAIN_RUNTIME_PASSWORD="${DATABRAIN_RUNTIME_PASSWORD:-runtimepassword}"
export DATABRAIN_TARGET_PASSWORD="${DATABRAIN_TARGET_PASSWORD:-targetpassword}"
export DATABRAIN_POSTGRES_PORT="${DATABRAIN_POSTGRES_PORT:-55432}"
export DATABRAIN_HTTP_PORT="${DATABRAIN_HTTP_PORT:-8080}"
export APP_ENV=development
export APP_ORIGIN="${APP_ORIGIN:-http://localhost:$DATABRAIN_HTTP_PORT}"
export HTTP_ADDR="127.0.0.1:$DATABRAIN_HTTP_PORT"
export APP_SECRET_KEY="${APP_SECRET_KEY:-kThZ7p4VzQx9mYbL2sW1fR8cE5vN0uI3jG6aD4eA7tM=}"
export TARGET_ALLOWED_HOSTS="${TARGET_ALLOWED_HOSTS:-127.0.0.1,localhost,::1}"

database_mode="${DATABRAIN_DATABASE_MODE:-docker}"
case "$database_mode" in
  docker) ;;
  existing) : "${PLATFORM_DATABASE_URL:?Mode existing memerlukan PLATFORM_DATABASE_URL}" "${MIGRATOR_DATABASE_URL:?Mode existing memerlukan MIGRATOR_DATABASE_URL}" ;;
  *) echo 'DATABRAIN_DATABASE_MODE harus docker atau existing.' >&2; exit 1 ;;
esac
npm --prefix frontend ci
npm --prefix frontend run typecheck
npm --prefix frontend run build
if [ "$database_mode" = docker ]; then
  export PLATFORM_DATABASE_URL="postgres://databrain_runtime@127.0.0.1:$DATABRAIN_POSTGRES_PORT/databrain_dev?sslmode=disable"
  export PLATFORM_DATABASE_PASSWORD="$DATABRAIN_RUNTIME_PASSWORD"
  export MIGRATOR_DATABASE_URL="postgres://databrain_migrator@127.0.0.1:$DATABRAIN_POSTGRES_PORT/databrain_dev?sslmode=disable"
  export MIGRATOR_DATABASE_PASSWORD="$DATABRAIN_MIGRATOR_PASSWORD"
  # Kedua mode berbagi process lock; hentikan app Docker sebelum server host.
  docker compose -f infra/compose.yaml stop app
  docker compose -f infra/compose.yaml up -d --wait postgres
fi
cd "$ROOT_DIR/backend"
export CGO_ENABLED=0
mkdir -p ../.tmp
go build -o ../.tmp/manage ./cmd/manage
go build -o ../.tmp/api ./cmd/api
../.tmp/manage migrate up
umask 077
password_file=$(mktemp)
trap 'rm -f "$password_file"' EXIT HUP INT TERM
printf '%s' "${BOOTSTRAP_ADMIN_PASSWORD:-AdminPassword123!}" > "$password_file"
../.tmp/manage bootstrap-admin --username "${BOOTSTRAP_ADMIN_USERNAME:-admin}" \
  --display-name "${BOOTSTRAP_ADMIN_DISPLAY_NAME:-Administrator}" --password-file "$password_file"
rm -f "$password_file"
trap - EXIT HUP INT TERM
echo "DataBrain: $APP_ORIGIN (admin: ${BOOTSTRAP_ADMIN_USERNAME:-admin})"
exec ../.tmp/api
