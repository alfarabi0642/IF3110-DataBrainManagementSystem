# Menjalankan DataBrain untuk development

Folder ini menyiapkan database lokal dan menjelaskan cara menjalankan aplikasi serta tes.

| File | Fungsi |
|---|---|
| `setup.sql` | Membuat role PostgreSQL, database platform `databrain_dev`, dan dua database demo `school_demo` serta `inventory_demo` pada cluster lokal yang baru. |
| `init-demo-journals.sh` | Memasang jurnal di kedua database demo setelah `setup.sql` selesai. |
| `target-journal.sql` | Membuat jurnal di database target untuk menyimpan bukti commit dan mendukung pemulihan eksekusi migrasi. |

## Menjalankan aplikasi

**Full Docker.** Pasang Docker dan Docker Compose, lalu jalankan dari root repository:

```sh
docker compose -f infra/compose.yaml up -d --build --wait
```

Buka [localhost:8080](http://localhost:8080). Akun awal untuk development lokal adalah `admin` dengan password `AdminPassword123!`. Kalau `APP_ORIGIN` diubah, buka aplikasi dengan hostname dan port yang sesuai.

Saat penyimpanan PostgreSQL masih kosong, container database menjalankan `setup.sql` lalu `init-demo-journals.sh`. Container aplikasi menjalankan `manage migrate up` untuk memasang skema platform dan antrean River, lalu membuat akun admin awal. Worker mulai setelah aplikasi siap. Untuk menghentikan layanan sambil menyimpan data:

```sh
docker compose -f infra/compose.yaml down
```

**Go di host, PostgreSQL di Docker.** Pasang Go 1.27.3, Node.js 22.12+, npm, dan Docker. Jalankan `bash ./run-dev.sh` di Linux/macOS atau `powershell -ExecutionPolicy Bypass -File ./run-dev.ps1` di Windows, dari root repository. Runner membangun frontend dan backend, menyiapkan database, lalu menjalankan API dan worker di host. Hentikan dengan Ctrl+C sebelum pindah ke full Docker.

## Yang bisa dicoba

1. Masuk sebagai admin, buat user dan grup, lalu tambahkan anggota dan pilih leader.
2. Daftarkan koneksi PostgreSQL dan beri grup hak `view_only` atau `view_edit`. Untuk database demo, gunakan host `postgres` dan port `5432` dalam mode full Docker, atau `127.0.0.1` dan port `55432` dalam mode host dengan konfigurasi default. Nama role targetnya `databrain_target`. Database demo dimulai tanpa tabel pengguna.
3. Masuk sebagai user yang punya akses. Buka Explorer untuk melihat skema, tabel, dan data target.
4. Dengan hak edit, buat draft SQL dan ajukan review. Minta leader dari setiap grup yang diwajibkan untuk menyetujui, lalu antrekan eksekusi sebagai pengaju. Pantau hasil di Execution dan lihat commit di History.
5. Coba rollback, forward, atau pemulihan batch ketika aksi tersebut tersedia. Untuk menyiapkan jurnal di target lain dan menangani hasil commit yang belum pasti, lihat [panduan operasional](../../docs/OPERATIONAL_RUNBOOK.md).

## Menjalankan tes

Dari root repository, gunakan fixture PostgreSQL terisolasi untuk tes backend dengan database nyata:

```sh
docker compose -p databrain-tests -f infra/test.compose.yaml run --rm tests
```

Untuk cek cepat backend dari `backend/`, jalankan `go test -count=1 ./...` dan `go vet ./...`. Tes integrasi database dilewati jika fixture belum dikonfigurasi.

Untuk frontend dari `frontend/`, jalankan `npm ci`, lalu `npm run typecheck`, `npm run test:run`, dan `npm run build`. `npm run test:e2e` memerlukan Chrome dan fixture PostgreSQL lokal yang terverifikasi. Hasil browser disimpan di `.tmp/e2e`.

## Memakai data yang sudah ada

Pertahankan volume, credential, dan `APP_SECRET_KEY` saat menjalankan ulang aplikasi. Migrasi platform saat ini mendukung database kosong atau upgrade dari M1 versi 1 ke versi 2. Database yang sudah memakai rangkaian M2 sebelum penggabungan memerlukan jalur upgrade terpisah jika datanya perlu dipertahankan. Untuk development dengan data disposable, volume baru memungkinkan instalasi bersih.
