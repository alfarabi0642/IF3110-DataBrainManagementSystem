// Package problem defines safe errors shared by backend features.
package problem

type Code string

const (
	PlatformUnavailable    Code = "PLATFORM_UNAVAILABLE"
	InvalidRequest         Code = "INVALID_REQUEST"
	BodyTooLarge           Code = "BODY_TOO_LARGE"
	ResultTooLarge         Code = "RESULT_TOO_LARGE"
	UnsupportedMediaType   Code = "UNSUPPORTED_MEDIA_TYPE"
	InternalError          Code = "INTERNAL_ERROR"
	RequestTimeout         Code = "REQUEST_TIMEOUT"
	ValidationFailed       Code = "VALIDATION_FAILED"
	RevisionConflict       Code = "REVISION_CONFLICT"
	SessionInvalid         Code = "SESSION_INVALID"
	CSRFInvalid            Code = "CSRF_INVALID"
	OriginRejected         Code = "ORIGIN_REJECTED"
	AdminRequired          Code = "ADMIN_REQUIRED"
	DatabaseAccessDenied   Code = "DATABASE_ACCESS_DENIED"
	DatabaseNotFound       Code = "DATABASE_NOT_FOUND"
	TargetConnectionFailed Code = "TARGET_CONNECTION_FAILED"
	TargetUnavailable      Code = "TARGET_UNAVAILABLE"
	TargetBusy             Code = "TARGET_BUSY"
	TargetConflict         Code = "TARGET_CONFLICT"
	RelationNotFound       Code = "RELATION_NOT_FOUND"
	RelationChanged        Code = "RELATION_CHANGED"
	CellTooLarge           Code = "CELL_TOO_LARGE"
	UnsupportedColumnType  Code = "UNSUPPORTED_COLUMN_TYPE"
	ExecutionCapacity      Code = "EXECUTION_CAPACITY"
)

// Error contains only a public code and a safe message. Driver errors, SQL,
// credentials, and session tokens must never be placed in these fields.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}
