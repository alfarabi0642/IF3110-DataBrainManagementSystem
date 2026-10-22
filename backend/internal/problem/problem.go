// Package problem defines safe errors shared by backend features.
package problem

type Code string

const PlatformUnavailable Code = "PLATFORM_UNAVAILABLE"

// Error contains only a public code and a safe message. Driver errors, SQL,
// credentials, and session tokens must never be placed in these fields.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Message
}
