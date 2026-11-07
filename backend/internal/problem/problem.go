// Package problem mendefinisikan error aman untuk fitur backend.
package problem

type Code string

const (
	PlatformUnavailable    Code = "PLATFORM_UNAVAILABLE"
	InvalidRequest         Code = "INVALID_REQUEST"
	RouteNotFound          Code = "ROUTE_NOT_FOUND"
	MethodNotAllowed       Code = "METHOD_NOT_ALLOWED"
	BodyTooLarge           Code = "BODY_TOO_LARGE"
	ResultTooLarge         Code = "RESULT_TOO_LARGE"
	UnsupportedMediaType   Code = "UNSUPPORTED_MEDIA_TYPE"
	InternalError          Code = "INTERNAL_ERROR"
	RequestTimeout         Code = "REQUEST_TIMEOUT"
	ValidationFailed       Code = "VALIDATION_FAILED"
	RevisionConflict       Code = "REVISION_CONFLICT"
	AuthBusy               Code = "AUTH_BUSY"
	AlreadyAuthenticated   Code = "ALREADY_AUTHENTICATED"
	InvalidCredentials     Code = "INVALID_CREDENTIALS"
	LoginRateLimited       Code = "LOGIN_RATE_LIMITED"
	UsernameTaken          Code = "USERNAME_TAKEN"
	GroupNameTaken         Code = "GROUP_NAME_TAKEN"
	PlatformLimitReached   Code = "PLATFORM_LIMIT_REACHED"
	LastAdmin              Code = "LAST_ADMIN"
	UserNotFound           Code = "USER_NOT_FOUND"
	GroupNotFound          Code = "GROUP_NOT_FOUND"
	SessionInvalid         Code = "SESSION_INVALID"
	ReauthRequired         Code = "REAUTH_REQUIRED"
	CSRFInvalid            Code = "CSRF_INVALID"
	OriginRejected         Code = "ORIGIN_REJECTED"
	AdminRequired          Code = "ADMIN_REQUIRED"
	DatabaseAccessDenied   Code = "DATABASE_ACCESS_DENIED"
	DatabaseNotFound       Code = "DATABASE_NOT_FOUND"
	DraftNotFound          Code = "DRAFT_NOT_FOUND"
	AttemptNotFound        Code = "ATTEMPT_NOT_FOUND"
	IdempotencyConflict    Code = "IDEMPOTENCY_CONFLICT"
	DraftAlreadyExecuted   Code = "DRAFT_ALREADY_EXECUTED"
	SQLInvalid             Code = "SQL_INVALID"
	SQLUnsupported         Code = "SQL_UNSUPPORTED"
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

// Error hanya berisi kode publik dan pesan aman. Error driver, SQL, credential, dan token session
// tidak boleh dimasukkan.
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
