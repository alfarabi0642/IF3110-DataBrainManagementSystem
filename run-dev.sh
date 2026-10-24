#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

echo "=========================================================="
echo "  DataBrain Management System - Local Development Runner  "
echo "=========================================================="

export PATH="/usr/local/go/bin:$PATH"

# 1. Konfigurasi Database Fixture
export DATABRAIN_FIXTURE_DATA_DIR="$ROOT_DIR/.tmp/postgres/data"
export DATABRAIN_FIXTURE_PASSWORD='localsecretpassword'
export DATABRAIN_OWNER_PASSWORD="$DATABRAIN_FIXTURE_PASSWORD"
export DATABRAIN_MIGRATOR_PASSWORD='migratorpassword'
export DATABRAIN_RUNTIME_PASSWORD='runtimepassword'
export DATABRAIN_TARGET_PASSWORD='targetpassword'

# 2. Konfigurasi Aplikasi Web
export APP_ENV=development
export APP_ORIGIN=http://localhost:8080
export HTTP_ADDR=127.0.0.1:8080
export PLATFORM_DATABASE_URL="postgres://databrain_runtime:${DATABRAIN_RUNTIME_PASSWORD}@127.0.0.1:55432/databrain_dev?sslmode=disable"
export MIGRATOR_DATABASE_URL="postgres://databrain_migrator:${DATABRAIN_MIGRATOR_PASSWORD}@127.0.0.1:55432/databrain_dev?sslmode=disable"
export APP_SECRET_KEY="${APP_SECRET_KEY:-kThZ7p4VzQx9mYbL2sW1fR8cE5vN0uI3jG6aD4eA7tM=}"
export TARGET_ALLOWED_HOSTS=127.0.0.1,localhost

mkdir -p "$DATABRAIN_FIXTURE_DATA_DIR"
mkdir -p "$ROOT_DIR/.tmp"

echo ""
echo "[1/5] Menyalakan PostgreSQL Container di port 55432..."
docker compose -f infra/compose.yaml up -d postgres --wait

echo ""
echo "[2/5] Memeriksa dan menginisialisasi database roles/schemas..."
# Periksa apakah database databrain_dev sudah ada
if ! PGPASSWORD="$DATABRAIN_FIXTURE_PASSWORD" psql -h 127.0.0.1 -p 55432 -U databrain_fixture_owner -d postgres -lqt 2>/dev/null | cut -d \| -f 1 | grep -qw databrain_dev; then
    echo "Menjalankan backend/dev/setup.sql..."
    PGPASSWORD="$DATABRAIN_FIXTURE_PASSWORD" psql -h 127.0.0.1 -p 55432 \
      -U databrain_fixture_owner -d postgres -f backend/dev/setup.sql || true
else
    echo "Database databrain_dev sudah terinisialisasi."
fi

cd "$ROOT_DIR/backend"

echo ""
echo "[3/5] Menerapkan platform migrations..."
go run ./cmd/manage migrate up

echo ""
echo "[4/5] Memeriksa akun Administrator..."
printf '%s' 'AdminPassword123!' > "$ROOT_DIR/.tmp/admin-password"
go run ./cmd/manage bootstrap-admin --username admin --display-name "Administrator" \
  --password-file "$ROOT_DIR/.tmp/admin-password" 2>/dev/null || echo "Akun admin sudah tersedia."

echo ""
echo "[5/5] Memulai server web DataBrain di http://localhost:8080"
echo "----------------------------------------------------------"
echo "  Login Info:"
echo "  URL      : http://localhost:8080"
echo "  Username : admin"
echo "  Password : AdminPassword123!"
echo "----------------------------------------------------------"
echo "Tekan Ctrl+C untuk menghentikan server."
echo ""

exec go run ./cmd/api
