#!/bin/sh
set -e

echo "=== Menerapkan migrasi database platform ==="
/bin/manage migrate up

if [ -n "$BOOTSTRAP_ADMIN_PASSWORD" ]; then
    echo "=== Memeriksa akun bootstrap admin ==="
    printf '%s' "$BOOTSTRAP_ADMIN_PASSWORD" > /tmp/admin-pass
    /bin/manage bootstrap-admin --username "${BOOTSTRAP_ADMIN_USERNAME:-admin}" \
        --display-name "${BOOTSTRAP_ADMIN_DISPLAY_NAME:-Administrator}" \
        --password-file /tmp/admin-pass 2>/dev/null || echo "Admin sudah diinisialisasi."
    rm -f /tmp/admin-pass
fi

echo "=== Memulai server DataBrain di ${HTTP_ADDR:-:8080} ==="
exec /bin/api
