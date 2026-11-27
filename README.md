# DataBrain Management System

DataBrain adalah aplikasi Go dan React untuk mengelola akses PostgreSQL, menjelajahi skema dan data, serta mengajukan dan menjalankan migrasi SQL dengan riwayat commit. Antarmuka web dan API JSON dilayani dari origin yang sama.

## Menjalankan aplikasi

Pilih mode **full Docker** atau **Go di host dengan PostgreSQL di Docker** sesuai [panduan development](backend/dev/README.md). Jalankan satu instance API per database platform. PostgreSQL 17 dan 18 didukung. Fixture Docker memakai PostgreSQL 17.11.

Untuk mencoba full Docker dari root repository:

```sh
docker compose -f infra/compose.yaml up -d --build --wait
```

Buka [localhost:8080](http://localhost:8080). Masuk dengan username `admin` dan password `AdminPassword123!`. Kredensial ini untuk development lokal. Pertahankan nilai `APP_SECRET_KEY` saat restart agar aplikasi tetap bisa membaca kredensial target dan token sesi yang tersimpan.

## Pengujian

Untuk memeriksa dan membangun frontend, gunakan Node.js 22.12+ dan npm. Jalankan perintah berikut dari `frontend/`.

```sh
npm ci
npm run typecheck
npm run test:run
npm run build
```

Saat dijalankan dari host, API memerlukan aset frontend di `frontend/dist`. Image Docker sudah menyertakan aset tersebut. Rute halaman mendukung akses langsung dan refresh, sedangkan `/api` mengembalikan JSON.

`npm run test:e2e` memerlukan Chrome, hasil build frontend, dan [fixture PostgreSQL lokal yang terverifikasi](backend/dev/README.md#menjalankan-tes). Screenshot dan hasil tes browser disimpan di `.tmp/e2e`.

Untuk development dengan Vite, jalankan backend dengan `APP_ORIGIN=http://127.0.0.1:5173`, lalu `npm run dev` dari `frontend/` dan buka origin tersebut. Runner `run-dev.ps1`/`run-dev.sh` mempertahankan override `APP_ORIGIN` dari environment; default tanpa override tetap `http://localhost:8080`. Proxy meneruskan `/api`, `/static` dan `/healthz` ke `http://127.0.0.1:8080`; atur `API_PROXY_TARGET` bila alamat backend berbeda. Proxy mempertahankan Origin browser; pemeriksaan session/CSRF tetap aktif. Jangan memakai origin backend untuk login ketika konfigurasi Origin menunjuk Vite.

API meliputi `/api/session`, `/api/login`, `/api/logout`, `/api/admin/{users,groups,databases}`, `/api/databases` dan resource target `/api/databases/{id}/{explorer,drafts,requests,history,activity,attempts,receipts,executions}`. Success memakai `{data,request_id}` (list menambah `page`); error memakai `{error:{code,message},request_id}` dengan HTTP status sebenarnya. API tidak melakukan redirect login. Bootstrap memberi state `anonymous`, `authenticated` atau `reauth_required`, identitas minimum, `csrf_token` dan `absolute_expires_at`; session tetap cookie HttpOnly.

Login menerima JSON `{username,password}`, logout menerima `{}`. Semua mutasi memerlukan tepat satu header `X-CSRF-Token` dari bootstrap dan Origin yang sama dengan `APP_ORIGIN`. Login gagal memakai 401 `INVALID_CREDENTIALS`, rate limit 429 `LOGIN_RATE_LIMITED` dengan `Retry-After`, session invalid 401 `SESSION_INVALID`, dan reauth 401 `REAUTH_REQUIRED` pada API terlindungi. Client membaca ulang bootstrap setelah mutasi, membersihkan protected cache saat logout/pergantian identitas atau family, dan tidak mengulang mutasi otomatis saat session/CSRF kedaluwarsa. Token CSRF hanya disimpan dalam memory browser.

Pemeriksaan cepat dari `backend/` memerlukan Go 1.27.3:

```sh
go test -count=1 ./...
go vet ./...
```

Tes integrasi PostgreSQL dilewati jika `DATABRAIN_TEST_ADMIN_URL` belum diatur. Untuk menjalankan suite backend dengan PostgreSQL terisolasi, jalankan perintah berikut dari root repository.

```sh
docker compose -p databrain-tests -f infra/test.compose.yaml run --rm tests
```

Runner menjalankan `go test -json -p 1 -count=1 ./...` dan `go vet ./...` di lingkungan tes yang terpisah dari database aplikasi.

## Contoh

Untuk mencoba satu migrasi di database demo lokal, masuk dengan username `admin` dan password `AdminPassword123!`.

1. Di **Admin**, buat dua akun pengguna dan satu grup. Masukkan kedua akun ke grup, lalu jadikan salah satunya leader. Akun lainnya akan menjadi pengaju migrasi.
2. Daftarkan koneksi ke `school_demo` dengan pengaturan di bawah. Beri grup akses **View and edit** (`view_edit`) ke koneksi itu.
3. Masuk sebagai pengaju, buka **Drafts** pada koneksi tersebut, lalu buat draft dengan SQL `up` dan `down` di bawah. Simpan dan ajukan review.
4. Masuk sebagai leader, buka **Requests**, lalu setujui permintaan. Pada target dengan beberapa grup, setiap grup aktif yang punya akses perlu memberikan persetujuan melalui leader-nya.
5. Masuk kembali sebagai pengaju. Buka permintaan yang disetujui dan klik **Queue approved execution**. Pantau status di **Execution**. Setelah berhasil, lihat catatan commit di **History** dan tabel baru di **Explorer**.

| Pengaturan koneksi | Full Docker | Go di host |
|---|---|---|
| Host | `postgres` | `127.0.0.1` |
| Port | `5432` | `55432` |
| Database | `school_demo` | `school_demo` |
| Username | `databrain_target` | `databrain_target` |
| Password lokal default | `targetpassword` | `targetpassword` |
| SSL mode | `disable` | `disable` |

Jika konfigurasi development diubah, gunakan port dan password target yang sesuai.

**SQL up**

```sql
CREATE TABLE public.contoh_migrasi (id bigint PRIMARY KEY);
```

**SQL down**

```sql
DROP TABLE public.contoh_migrasi;
```

## Kebijakan Session

Server menyimpan sesi dan memberi browser ID sesi melalui cookie. Sesi anonim berlaku selama 10 menit. Setelah login, sesi berakhir setelah 30 menit tanpa aktivitas atau setelah mencapai umur maksimum 8 jam. Aktivitas memperbarui batas waktu tanpa aktivitas. Pengguna harus login kembali setelah sesi berakhir.

Identifier dirotasi setiap 15 menit. Identifier lama menjadi alias selama 120 detik, paling lama sampai batas maksimum session. Request paralel dengan alias yang masih valid menerima identifier terkini. Rotasi tidak memperpanjang absolute expiration.

Login mencabut family anonymous atau family yang memerlukan reauthentication, kemudian membuat family authenticated baru dalam transaksi yang sama. Token anonymous lama langsung tidak berlaku dan tidak memperoleh hak authenticated.

Peningkatan privilege (misalnya promosi admin, penambahan membership/grant, atau reaktivasi akses) merotasi family yang masih hidup dan mewajibkan verifikasi password sebelum fitur terlindungi dapat dipakai lagi. Batas waktu lama dipertahankan saat rotasi. Login ulang yang berhasil membuat family baru. Penurunan/pencabutan akses dan reset password mencabut seluruh family pengguna terkait serta menaikkan security version. Perubahan nama dan pengiriman nilai identik tidak mengubah session. Perubahan tipe akun yang tetap nonaktif hanya menyimpan data akun.

Cookie di development dan test bernama `databrain_sid`. Production memakai `__Host-databrain_sid`. Keduanya `HttpOnly`, `SameSite=Lax`, `Path=/`, tanpa Domain. Cookie memakai flag `Secure` pada koneksi HTTPS. Production mewajibkan HTTPS. HTTP hanya diterima pada origin loopback development/test. Logout mencabut family beserta semua alias di backend sebelum cookie dihapus. Request yang mengubah state memerlukan pemeriksaan CSRF dan Origin.

## Aturan akses

Akses database berasal dari keanggotaan pengguna di **grup aktif** yang memiliki grant pada **target aktif**. Jika pengguna memperoleh beberapa grant pada target yang sama, tingkat akses tertinggi berlaku. Satu grant `view_edit` memberi hak editor. Jika semua grant hanya `view_only`, pengguna mendapat hak viewer. Akses dihitung terpisah untuk setiap database.

Viewer dapat membaca schema, data, history, dan detail migration. Editor dapat membuat, memperbarui, dan mengajukan draft untuk review. Pengaju dapat memasukkan operasi ke antrean setelah semua persetujuan wajib tercatat. Platform admin mengelola pengguna, grup, koneksi, dan grant. Untuk mengakses database target, admin tetap membutuhkan grant yang efektif. Semua tindakan diperiksa di backend.

Aplikasi menerima subset SQL PostgreSQL, termasuk `CREATE`, `ALTER`, dan `DROP TABLE`, `CREATE` dan `DROP SCHEMA`, sebagian operasi indeks, serta `INSERT`, `UPDATE`, dan `DELETE`. Kontrol transaksi, fungsi atau prosedur, dan `SELECT` sebagai perintah utama ditolak sebelum eksekusi. Draft dapat disimpan sebelum SQL siap diajukan.

Lihat status di **Execution** dan hasil commit di **History**. Jika hasil `COMMIT` belum pasti, tunggu hasil rekonsiliasi sebelum menjalankan ulang SQL. Operator menggunakan `manage resolve-attempt` untuk attempt lama (M1) setelah API dihentikan. Untuk eksekusi asinkron (M2), `manage reconcile-execution --execution <UUID>` membaca bukti dari jurnal database target.

## Logging dan Tracing

API (`app`) dan worker (`worker`) menulis structured log JSON (`log/slog`) dan span OpenTelemetry (`stdouttrace`) ke `stderr`. 

Untuk melihat log dan span secara langsung pada mode Docker Compose:

```sh
# Log API dan worker bersamaan
docker compose -f infra/compose.yaml logs -f --tail=50 app worker

# Filter event migrasi, lock, batch, dan SQL
docker compose -f infra/compose.yaml logs --tail=200 app worker | grep -E "migration execution|migration batch|SQL query|migration\.lock|migration\.batch"

# Lacak satu eksekusi lintas proses menggunakan trace_id
docker compose -f infra/compose.yaml logs app worker | grep "<TRACE_ID>"
```

Pada mode host (`bash ./run-dev.sh`), log JSON dan span langsung tampil pada `stderr` terminal.

**Contoh output structured log JSON (korelasi eksekusi & SQL):**

```json
{"time":"2026-11-27T11:15:02Z","level":"INFO","event":"migration execution started","service":"databrain-worker","execution_id":"9a2c...","request_id":"4f1b...","database_id":"1d8e...","actor_id":"7c3a...","migration_id":"2b9f...","job_id":42,"attempt":1,"direction":"apply","mode":"normal","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}
{"time":"2026-11-27T11:15:02Z","level":"INFO","event":"SQL query completed","service":"databrain-worker","operation":"CREATE","sql_template":"CREATE TABLE public.contoh_migrasi (id bigint PRIMARY KEY)","sql_hash":"a1b2c3...","args":[],"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"5b4185666d50f68b","execution_id":"9a2c...","request_id":"4f1b...","job_id":42,"attempt":1,"database_id":"1d8e...","actor_id":"7c3a...","migration_id":"2b9f...","status":"ok","duration_ms":4,"rows":0}
```

**Contoh rantai span OpenTelemetry (propagasi W3C `traceparent` melalui antrean River):**

```json
{"Name":"http.request","SpanContext":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"1111111111111111"},"Attributes":[{"Key":"http.method","Value":{"Type":"STRING","Value":"POST"}},{"Key":"http.route","Value":{"Type":"STRING","Value":"POST /api/databases/{id}/requests/{request_id}/execute"}},{"Key":"http.status_code","Value":{"Type":"INT64","Value":202}}]}
{"Name":"river.enqueue","SpanContext":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"2222222222222222"},"Parent":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"1111111111111111"},"Attributes":[{"Key":"execution_id","Value":{"Type":"STRING","Value":"9a2c..."}},{"Key":"job_id","Value":{"Type":"INT64","Value":42}},{"Key":"status","Value":{"Type":"STRING","Value":"enqueued"}}]}
{"Name":"river.consume","SpanContext":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"3333333333333333"},"Parent":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"2222222222222222"},"Attributes":[{"Key":"execution_id","Value":{"Type":"STRING","Value":"9a2c..."}},{"Key":"job_id","Value":{"Type":"INT64","Value":42}},{"Key":"attempt","Value":{"Type":"INT64","Value":1}},{"Key":"status","Value":{"Type":"STRING","Value":"ok"}}]}
{"Name":"migration.lock","SpanContext":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"4444444444444444"},"Parent":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"3333333333333333"},"Attributes":[{"Key":"lock.acquired","Value":{"Type":"BOOL","Value":true}},{"Key":"status","Value":{"Type":"STRING","Value":"acquired"}}]}
```

