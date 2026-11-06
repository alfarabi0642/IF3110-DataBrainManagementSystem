# Development dan fixture pengujian

Jalankan perintah Docker dari root repository. Docker Desktop dengan Linux containers dan Docker Compose diperlukan pada Windows. Mode full Docker tidak memerlukan Go atau Node.js di host. Mode host memerlukan Go 1.27.3, Node.js 22.12+ dan npm; gunakan PowerShell di Windows atau Bash pada Linux/macOS.

## Mode 1: full Docker

```sh
docker compose -f infra/compose.yaml up -d --build --wait
```

Buka http://localhost:8080. Build context adalah root repository; stage Node membangun React, stage Go membangun API/CLI dan image akhir melayani SPA, static asset serta `/api` pada satu origin. Compose menginisialisasi role dan database **sekali**, saat volume kosong, melalui `setup.sql`. Entrypoint app menjalankan migration platform dan bootstrap admin; admin aktif yang sudah ada adalah hasil idempoten. Password tidak valid, koneksi gagal, atau error lain menghentikan startup dengan pesan aman. Jangan menjalankan setup.sql atau server host bersamaan.

Restart dengan perintah yang sama, atau `docker compose -f infra/compose.yaml restart app`. Hentikan dengan `docker compose -f infra/compose.yaml down` tanpa `-v`; volume dan School Demo tetap tersimpan.

## Mode 2: Go host + PostgreSQL Docker

Dari root, jalankan salah satu:

```powershell
powershell -ExecutionPolicy Bypass -File ./run-dev.ps1
```

```sh
bash ./run-dev.sh
```

Runner menjalankan `npm ci`, typecheck dan build frontend, lalu menghentikan service `app` pada project Compose yang dipilih, menyalakan **hanya postgres**, membangun binary Go non-CGO, menjalankan migration dan bootstrap, lalu server host di http://localhost:8080. Setup SQL tidak dijalankan ulang. Runner berhenti jika sebuah langkah gagal, dan tidak mencetak password. Hentikan server host dengan Ctrl+C sebelum kembali ke full Docker. Hanya satu server per platform DB yang dapat memegang process lock; menyalakan instance kedua akan ditolak. Jangan membuka runner kedua pada fixture yang sama.

Go dijalankan dari `backend/`; default asset directory adalah `../frontend/dist` dan `web/static`. Override dengan `SPA_DIR` dan `STATIC_DIR` bila menjalankan binary dari direktori lain. Build SPA dengan `index.html` wajib tersedia saat startup; tidak ada fallback ke aplikasi HTML lama. Binary dibangun dahulu sehingga exit code CLI bootstrap diterima langsung oleh script. App tidak otomatis membaca `.env`.

Untuk PostgreSQL yang sudah disiapkan di host, set `DATABRAIN_DATABASE_MODE=existing`, `PLATFORM_DATABASE_URL`, `MIGRATOR_DATABASE_URL`, password terpisah yang sesuai, dan `APP_SECRET_KEY` instalasi tersebut sebelum menjalankan runner yang sama. Mode ini tidak memanggil Docker atau setup.sql; runtime dan migrator tetap harus role terpisah. Mode existing menolak kunci yang tidak disetel agar tidak mengganti kunci instalasi secara diam-diam. `TARGET_ALLOWED_HOSTS` harus memuat hostname target sesuai environment; target ber-host `postgres` di network Docker tidak otomatis dapat diakses dari host.

## Konfigurasi dan credential

Default lokal: owner `localsecretpassword`, migrator `migratorpassword`, runtime `runtimepassword`, target `targetpassword`; admin awal `admin` / `AdminPassword123!`. Semua nilai ini contoh development. Override **sebelum** volume pertama dibuat. Contoh PowerShell dengan karakter khusus:

```powershell
$env:DATABRAIN_RUNTIME_PASSWORD = 'runtime@:/?#%$!custom'
$env:DATABRAIN_MIGRATOR_PASSWORD = 'migrator@:/?#%$!custom'
$env:DATABRAIN_FIXTURE_PASSWORD = 'owner@:/?#%$!custom'
$env:DATABRAIN_TARGET_PASSWORD = 'target@:/?#%$!custom'
$env:BOOTSTRAP_ADMIN_PASSWORD = 'Choose-a-private-admin-password'
$env:COMPOSE_PROJECT_NAME = 'databrain-local'
$env:DATABRAIN_POSTGRES_PORT = '55434'
$env:DATABRAIN_HTTP_PORT = '8082'
./run-dev.ps1
```

Di Bash gunakan `export NAMA='nilai'`. Compose menerima variabel yang sama, termasuk saat full Docker. Untuk full Docker, alternatifnya simpan nilai di file env lokal lalu pakai `docker compose --env-file PATH -p PROJECT -f infra/compose.yaml ...` pada setiap perintah. Env file bukan konfigurasi otomatis Go; runner host membaca environment proses. Jangan menyimpan file credential di Git. Named volume default mengikuti project; `DATABRAIN_FIXTURE_DATA_DIR` opsional menggantinya dengan bind mount. Jangan menunjuk fixture baru ke data pengguna.

Runtime memakai `PLATFORM_DATABASE_URL` dan password mentah `PLATFORM_DATABASE_PASSWORD`; migrator memakai `MIGRATOR_DATABASE_URL` dan `MIGRATOR_DATABASE_PASSWORD`. Compose/runner meneruskan nilai yang sama dengan password role PostgreSQL. Go meng-encode password memakai `url.UserPassword`, sehingga `@`, `:`, `/`, `?`, `#`, `%`, `$`, spasi, dan Unicode tidak mengubah struktur URI. Jika variabel password terpisah tidak disetel, URL lengkap lama tetap didukung; encode credential URI secara benar. Jangan memakai `docker compose config` dalam log publik karena hasil render mengandung environment rahasia.

Mengubah environment pada **volume lama tidak mengubah password role**. Untuk volume lama, pertahankan nilai awal atau ubah password role secara sengaja melalui operasi PostgreSQL yang aman lalu selaraskan konfigurasi. Jangan menghapus volume untuk mengatasi mismatch credential.

`APP_SECRET_KEY` harus tetap sama pada kedua mode dan setiap restart. Default fixture lokal sengaja stabil. Untuk deployment sendiri, buat base64 standar dari 32 byte acak dan simpan satu kali dalam konfigurasi rahasia; jangan menghasilkan kunci baru setiap start. Pergantian kunci membuat password koneksi tersimpan dan canonical token lama tidak terbaca. Override identik harus dipakai oleh full Docker maupun runner host.

## Upgrade database yang sudah berisi data

Pertahankan project/volume, credential role dan `APP_SECRET_KEY` awal. Hentikan server lama, lalu jalankan mode yang sama seperti instalasi pertama. Entrypoint/runner hanya menjalankan `manage migrate up` dan bootstrap idempoten; migration yang sudah diterapkan tidak diulang. Schema aplikasi saat ini tetap version 1; user/group/membership/grant, koneksi terenkripsi, draft dan immutable attempt lama dipakai langsung. Jangan menjalankan setup.sql lagi, reset/drop database, atau menghapus volume saat upgrade.

Runtime `databrain_runtime` tidak boleh menjadi owner schema/database, superuser, CREATEDB, CREATEROLE atau BYPASSRLS. `databrain_migrator` memiliki schema dan menerapkan migration/grants; role target terpisah hanya memperoleh hak target. Instalasi yang belum memiliki role/grants tersebut harus menyiapkannya sebagai operasi administrasi PostgreSQL; mengganti URL runtime dengan owner bukan perbaikan startup.

## Frontend dan browser suite

Dari `frontend/`, jalankan `npm ci`, `npm run typecheck`, `npm run test:run` dan `npm run build`. Untuk Vite, pakai `APP_ORIGIN=http://127.0.0.1:5173` pada backend dan buka origin Vite; `API_PROXY_TARGET` memilih alamat backend. Seluruh mutasi tetap memakai cookie, Origin dan CSRF yang sama.

`npm run test:e2e` menggunakan Chrome yang terpasang (atau path `PUPPETEER_EXECUTABLE_PATH`) dan fixture integration yang sama dengan suite Go. Go dan Node.js harus tersedia pada PATH. Suite tidak menerima URL aplikasi/demo: fixture memeriksa owner `databrain_fixture_owner`, database `postgres`, port loopback 55432 dan data directory repository `.tmp/postgres/data` sebelum membuat database acak miliknya. Browser menguji login, administrasi, explorer, draft/conflict, review/execute/replay, history, direct URL/refresh/Back/Forward, expiry serta desktop/mobile light/dark. Hasil lokal ada di `.tmp/e2e`.

Untuk run host, gunakan cluster **disposable terpisah** pada path/port tersebut, dengan role dari setup.sql, lalu set environment berikut dengan credential fixture aktual (contoh PowerShell):

```powershell
$env:CGO_ENABLED = '0'
$env:DATABRAIN_TEST_ADMIN_URL = 'postgres://databrain_fixture_owner:test-owner-password@127.0.0.1:55432/postgres?sslmode=disable'
$env:DATABRAIN_OWNER_PASSWORD = 'test-owner-password'
$env:DATABRAIN_MIGRATOR_PASSWORD = 'test-migrator-password'
$env:DATABRAIN_RUNTIME_PASSWORD = 'test-runtime-password'
$env:DATABRAIN_TARGET_PASSWORD = 'test-target-password'
npm --prefix frontend run build
npm --prefix frontend run test:e2e
```

Pada Bash, gunakan `export` untuk variabel yang sama. Jangan gunakan cluster Compose aplikasi untuk command host ini. Fixture Docker integration di bawah tidak memerlukan Chrome/Node; browser suite host memakai cluster host tersendiri agar path guard tetap benar. Tanpa `DATABRAIN_TEST_ADMIN_URL`, command browser gagal; unit/integration Go biasa tetap melaporkan skip yang eksplisit.

Jika cluster host disposable belum ada, lakukan inisialisasi **sekali** dari root dengan binary PostgreSQL 17 pada PATH. `initdb` menolak direktori yang sudah berisi cluster; setup.sql juga menolak role/database yang sudah ada. Contoh ini memakai credential fixture dari blok di atas:

```powershell
New-Item -ItemType Directory -Force .tmp/postgres | Out-Null
[IO.File]::WriteAllText("$PWD/.tmp/postgres/owner-password.txt", $env:DATABRAIN_OWNER_PASSWORD)
initdb -D .tmp/postgres/data -U databrain_fixture_owner -A scram-sha-256 --pwfile=.tmp/postgres/owner-password.txt --encoding=UTF8 --locale=C
if ($LASTEXITCODE) { throw 'initdb gagal; jangan reset cluster yang sudah ada' }
Remove-Item -LiteralPath .tmp/postgres/owner-password.txt
pg_ctl -D .tmp/postgres/data -l .tmp/postgres/server.log -o '-h 127.0.0.1 -p 55432' -w start
$env:PGPASSWORD = $env:DATABRAIN_OWNER_PASSWORD
$env:DATABRAIN_FIXTURE_DATA_DIR = (Resolve-Path .tmp/postgres/data).Path.Replace('\','/')
psql -X -h 127.0.0.1 -p 55432 -U databrain_fixture_owner -d postgres -f backend/dev/setup.sql
```

Pada run berikutnya cukup `pg_ctl -D .tmp/postgres/data -l .tmp/postgres/server.log -o '-h 127.0.0.1 -p 55432' -w start`. Setelah suite selesai, hentikan **cluster fixture ini** dengan `pg_ctl -D .tmp/postgres/data -m fast -w stop`; data fixture boleh tetap ada untuk rerun. Jangan memakai path atau port cluster aplikasi.

## Komposisi runtime

`cmd/api/main.go` memuat config, membuka platform pool/process lock, menjalankan recovery, lalu mendaftarkan session/admin/target/explorer/migration API pada satu `apiMux`. Feature API menambahkan registrasinya di titik ini dengan middleware session/CSRF dan Authorizer yang sama. Config lintas proses tersedia melalui `LoadPlatformDatabaseURL`, `LoadSecretKey` dan `LoadTargetSettings`; `Load` menyediakan pengaturan HTTP/asset API. `migration.NewAPIHandler` tidak memerlukan template. Runtime sekarang tetap satu server sinkron per platform DB; tidak ada flag worker atau queue yang berpura-pura aktif.

## Seluruh integration test dengan database nyata

Dari fresh checkout, satu perintah berikut bekerja di PowerShell maupun Bash:

```sh
docker compose -p databrain-tests -f infra/test.compose.yaml run --rm tests
```

Fixture khusus ini tidak membuka port host dan tidak memakai volume aplikasi. Go dan PostgreSQL berbagi network namespace loopback `127.0.0.1:55432`. Root sumber `/workspace` cocok dengan data directory server `/workspace/.tmp/postgres/data`, termasuk pada checkout Windows. Guard role owner, database `postgres`, port, dan direktori tetap aktif. Test membuat database dengan UUID sendiri, memeriksa ownership sebelum menghapusnya, dan tidak menyentuh School Demo. Host authentication memakai SCRAM agar percobaan password salah benar-benar ditolak.

Suite dijalankan serial per package (`-p 1`) untuk membatasi beban PostgreSQL dan memori test hash/parser; lalu `go vet ./...`. Tidak ada filter/skip tambahan. Tanpa variabel test DB pada run host biasa, integration test memang dilewati; hasil tersebut harus dilaporkan terpisah. Jangan memakai app Compose biasa untuk full integration test dari Windows: data directory container berbeda dari path host yang diharapkan guard.

Ulangi perintah yang sama untuk rerun. Bersihkan **hanya fixture test** setelah selesai:

```sh
docker compose -p databrain-tests -f infra/test.compose.yaml down -v
```

Jangan mengganti nama project atau file Compose pada perintah cleanup dengan project aplikasi. Volume test menyimpan cache Go dan cluster disposable; seluruhnya dapat dibuat ulang. Untuk memeriksa guard, ubah URL owner/port/database atau jalankan test dari path sumber yang tidak cocok: test harus gagal sebelum membuat database, bukan diluluskan dengan menonaktifkan guard.
