#!/bin/sh
set -e

echo "=== Menerapkan migrasi database platform ==="
/bin/manage migrate up

if [ -n "${BOOTSTRAP_ADMIN_PASSWORD:-}" ]; then
    echo "=== Memeriksa akun bootstrap admin ==="
    umask 077
    password_file=$(mktemp)
    trap 'rm -f "$password_file"' EXIT HUP INT TERM
    printf '%s' "$BOOTSTRAP_ADMIN_PASSWORD" > "$password_file"
    /bin/manage bootstrap-admin --username "${BOOTSTRAP_ADMIN_USERNAME:-admin}" \
        --display-name "${BOOTSTRAP_ADMIN_DISPLAY_NAME:-Administrator}" \
        --password-file "$password_file"
    rm -f "$password_file"
    trap - EXIT HUP INT TERM
fi

echo "=== Memulai server DataBrain di ${HTTP_ADDR:-:8080} ==="
exec /bin/api
