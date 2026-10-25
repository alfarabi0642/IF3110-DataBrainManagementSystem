# Development dan fixture pengujian

Jalankan perintah Docker dari root repository. Docker Desktop dengan Linux containers dan Docker Compose diperlukan pada Windows. Mode full Docker tidak memerlukan Go, psql, atau OpenSSL di host. Mode host memerlukan Go 1.27.3; gunakan PowerShell di Windows atau Bash pada Linux/macOS.

## Mode 1: full Docker

```sh
docker compose -f infra/compose.yaml up -d --build --wait
```

Buka http://localhost:8080. Compose menginisialisasi role dan database **sekali**, saat volume kosong, melalui `setup.sql`. Entrypoint app menjalankan migration platform dan bootstrap admin; admin aktif yang sudah ada adalah hasil idempoten. Password tidak valid, koneksi gagal, atau error lain menghentikan startup dengan pesan aman. Jangan menjalankan setup.sql atau server host bersamaan.

Restart dengan perintah yang sama, atau `docker compose -f infra/compose.yaml restart app`. Hentikan dengan `docker compose -f infra/compose.yaml down` tanpa `-v`; volume dan School Demo tetap tersimpan.

## Mode 2: Go host + PostgreSQL Docker

Dari root, jalankan salah satu:

```powershell
powershell -ExecutionPolicy Bypass -File ./run-dev.ps1
```

```sh
bash ./run-dev.sh
```

Runner menghentikan service `app` pada project Compose yang dipilih, menyalakan **hanya postgres**, membangun binary Go, menjalankan migration dan bootstrap, lalu server host di http://localhost:8080. Setup SQL tidak dijalankan ulang. Runner berhenti jika sebuah langkah gagal, dan tidak mencetak password. Hentikan server host dengan Ctrl+C sebelum kembali ke full Docker. Hanya satu server per platform DB yang dapat memegang process lock; menyalakan instance kedua akan ditolak. Jangan membuka runner kedua pada fixture yang sama.

Go dijalankan dari `backend/` agar template dan static asset ditemukan. Binary dibangun dahulu sehingga exit code CLI bootstrap diterima langsung oleh script. App tidak otomatis membaca `.env`.

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
