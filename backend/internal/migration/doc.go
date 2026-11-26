// Package migration mengelola draft, persetujuan, penerimaan eksekusi, dan riwayat migrasi.
// Service menerima principal tervalidasi serta ID target eksplisit. Worker memanggil
// RevalidateOperation untuk memeriksa izin aktor dan keadaan target sebelum menjalankan SQL.
//
// Package httpapi menangani HTTP, sedangkan worker menjalankan SQL dan rekonsiliasi.
// Keduanya memakai package ini. Package execstore menyimpan eksekusi dan memasukkan job,
// head mengelola posisi migrasi dan finalisasi riwayat, journal mengelola lock dan bukti
// commit pada target, sqlpolicy memvalidasi SQL, dan legacy menangani eksekusi M1.
package migration
