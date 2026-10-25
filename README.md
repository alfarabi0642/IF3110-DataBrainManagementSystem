# DataBrain Management System

Aplikasi Go dengan HTML dari server untuk mengatur akses PostgreSQL, melihat schema/data, dan menjalankan migration transaksional dengan riwayat permanen. Alur inti dapat dipakai tanpa JavaScript.

## Menjalankan aplikasi

Pilih satu mode di [panduan development](backend/dev/README.md): **full Docker** atau **Go di host + PostgreSQL Docker**. Jangan menjalankan kedua server pada platform DB yang sama. PostgreSQL yang didukung: 17 dan 18; fixture memakai 17.11.

Untuk mencoba full Docker dari root repository:

```sh
docker compose -f infra/compose.yaml up -d --build --wait
```

Buka [localhost:8080](http://localhost:8080). Akun development awal adalah `admin`, password `AdminPassword123!`. Nilai contoh ini hanya untuk fixture lokal. Runner hanya menampilkan URL dan username. `APP_SECRET_KEY` harus tetap sama saat restart agar credential target dan token session tetap dapat dibaca.

## Pengujian

Pemeriksaan cepat dari `backend/` memerlukan Go 1.27.3:

```sh
go test -count=1 ./...
go vet ./...
```

Tanpa `DATABRAIN_TEST_ADMIN_URL`, integration test PostgreSQL dilewati secara eksplisit. Untuk seluruh suite dengan PostgreSQL nyata, jalankan dari root (PowerShell, Bash, dan Docker Compose):

```sh
docker compose -p databrain-tests -f infra/test.compose.yaml run --rm tests
```

Perintah ini menjalankan `go test -json -p 1 -count=1 ./...` lalu `go vet ./...`. Fixture terpisah, tanpa port host, tetap memeriksa role, database, data directory cluster, nama database acak, dan ownership sebelum cleanup. Lihat panduan development untuk mengulang dan membersihkan fixture test.

## Kebijakan session

Session disimpan di server; browser menerima identifier opaque lewat cookie. Session anonymous berumur 10 menit. Setelah login, idle timeout adalah 30 menit dan umur maksimum 8 jam. Aktivitas memperbarui idle, tetapi tidak memperpanjang batas maksimum. Setelah habis, pengguna harus login kembali.

Identifier dirotasi setiap 15 menit. Identifier lama menjadi alias selama 120 detik, paling lama sampai batas maksimum session. Request paralel dengan alias yang masih valid menerima identifier terkini. Rotasi tidak memperpanjang absolute expiration.

Login mencabut family anonymous atau family yang memerlukan reauthentication, kemudian membuat family authenticated baru dalam transaksi yang sama. Token anonymous lama langsung tidak berlaku dan tidak memperoleh hak authenticated.

Peningkatan privilege (misalnya promosi admin, penambahan membership/grant, atau reaktivasi akses) merotasi family yang masih hidup dan mewajibkan verifikasi password sebelum fitur terlindungi dapat dipakai lagi. Deadline lama dipertahankan selama rotasi; login ulang yang berhasil membuat family baru. Penurunan/pencabutan akses dan reset password mencabut seluruh family pengguna terkait serta menaikkan security version. Perubahan nama dan pengiriman nilai identik tidak mengubah session. Perubahan tipe akun yang tetap nonaktif hanya menyimpan data akun.

Cookie development/test bernama `databrain_sid`; production memakai `__Host-databrain_sid`. Keduanya `HttpOnly`, `SameSite=Lax`, `Path=/`, tanpa Domain. HTTPS memberi flag `Secure`; production mewajibkan HTTPS. HTTP hanya diterima pada origin loopback development/test. Logout mencabut family beserta semua alias di backend sebelum cookie dihapus. Request yang mengubah state memerlukan pemeriksaan CSRF dan Origin.

## Aturan akses

Akses database berasal dari membership user pada **group aktif**, grant group pada target, dan **target aktif**. Tanpa grant efektif, akses ditolak. Jika beberapa group mengakses target yang sama, tingkat paling permisif berlaku: satu `view_edit` cukup untuk menjadi editor; bila seluruh grant hanya `view_only`, pengguna menjadi viewer. Contoh: group Pembaca memberi View Only dan Pengembang memberi View + Edit pada database A; anggota keduanya mendapat View + Edit pada A. Grant pada database lain dihitung terpisah.

Viewer dapat membaca schema, data, history, dan detail migration. Editor juga dapat membuat, memperbarui, serta menjalankan draft. Platform admin mengelola user, group, koneksi, dan grant; status admin **tidak otomatis memberi akses database**. Admin tetap memerlukan membership/grant yang sama untuk membaca atau mengubah target. Semua tindakan diperiksa di backend.

Migration menjalankan subset SQL PostgreSQL yang diterima policy aplikasi: CREATE/ALTER/DROP TABLE, CREATE/DROP SCHEMA, sebagian INDEX, dan INSERT/UPDATE/DELETE. Transaction control, fungsi/prosedur, top-level SELECT, dan beberapa varian DDL ditolak sebelum eksekusi. Draft boleh menyimpan SQL yang belum valid. Riwayat dan receipt membedakan sukses, gagal, dan hasil `unknown`; jangan mengulang SQL saat hasil COMMIT belum pasti. Server dibatasi satu instance per platform DB melalui process lock. Recovery operator dilakukan dalam maintenance setelah server dihentikan.
