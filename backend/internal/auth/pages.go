package auth

import "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"

// LoginPage deliberately has no password field. Failed login attempts may
// redisplay only the normalized username and a fixed public error.
type LoginPage struct {
	httpx.Layout
	Username string
	Error    string
	Reauth   bool
}
