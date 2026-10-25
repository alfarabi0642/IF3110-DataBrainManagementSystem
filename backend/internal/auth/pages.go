package auth

import "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"

// LoginPage tidak memuat password. Login gagal hanya boleh menampilkan ulang username yang
// dinormalisasi dan pesan publik tetap.
type LoginPage struct {
	httpx.Layout
	Username string
	Error    string
	Reauth   bool
}
