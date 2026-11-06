// Package migration mengelola draft, eksekusi transaksional, riwayat, dan recovery migration.
// Service menerima principal tervalidasi dan target eksplisit; adapter HTTP menangani input dan response.
// Runner memakai context caller, koneksi target dedicated, serta snapshot dari reservasi tersimpan.
// markCommit mencatat fase sebelum COMMIT; finishExecution menyimpan evidence dan menjaga blokir unknown.
// Startup wajib memperoleh platformdb.Open lalu RecoverInterrupted sebelum menerima operasi.
// Saat shutdown, caller menguras operasi sebelum DB.Close; hilangnya DB.Lost membatalkan operasi aktif.
package migration
