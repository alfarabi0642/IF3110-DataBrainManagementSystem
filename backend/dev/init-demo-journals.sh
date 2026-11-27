#!/usr/bin/env bash
set -euo pipefail
# Provision tamper-proof databrain_runtime target journals on the two local demo target databases
# created by 01-setup.sql so a fresh `docker compose up` can run async migrations immediately.
for db in school_demo inventory_demo; do
  psql -X -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$db" -f /opt/databrain/target-journal.sql
done
