package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestErrorInfoRedactsRawMessages(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
	}{
		{&problem.Error{Code: problem.InvalidRequest, Message: "SENSITIVE"}, 400},
		{&problem.Error{Code: problem.RouteNotFound}, 404},
		{&problem.Error{Code: problem.MethodNotAllowed}, 405},
		{&problem.Error{Code: problem.CSRFInvalid}, 403},
		{&problem.Error{Code: problem.SessionInvalid}, 401},
		{&problem.Error{Code: problem.OriginRejected}, 403},
		{&problem.Error{Code: problem.AdminRequired}, 403},
		{&problem.Error{Code: problem.DatabaseAccessDenied}, 403},
		{&problem.Error{Code: problem.DatabaseNotFound}, 404},
		{&problem.Error{Code: problem.RevisionConflict}, 409},
		{&problem.Error{Code: problem.AlreadyAuthenticated}, 409},
		{&problem.Error{Code: problem.UsernameTaken}, 409},
		{&problem.Error{Code: problem.GroupNameTaken}, 409},
		{&problem.Error{Code: problem.PlatformLimitReached}, 409},
		{&problem.Error{Code: problem.LastAdmin}, 409},
		{&problem.Error{Code: problem.UserNotFound}, 404},
		{&problem.Error{Code: problem.GroupNotFound}, 404},
		{&problem.Error{Code: problem.AuthBusy}, 503},
		{&problem.Error{Code: problem.IdempotencyConflict, Message: "SENSITIVE"}, 409},
		{&problem.Error{Code: problem.DraftAlreadyExecuted}, 409},
		{&problem.Error{Code: problem.ExecutionCapacity}, 409},
		{&problem.Error{Code: problem.SQLInvalid, Message: "SENSITIVE"}, 422},
		{&problem.Error{Code: problem.SQLUnsupported}, 422},
		{&problem.Error{Code: problem.AttemptNotFound}, 404},
		{&problem.Error{Code: problem.TargetBusy, Message: "SENSITIVE"}, 409},
		{&problem.Error{Code: problem.TargetConflict, Message: "SENSITIVE"}, 409},
		{&problem.Error{Code: problem.TargetConnectionFailed, Message: "SENSITIVE"}, 422},
		{&problem.Error{Code: problem.TargetUnavailable, Message: "SENSITIVE"}, 503},
		{&problem.Error{Code: problem.RelationNotFound, Message: "SENSITIVE"}, 404},
		{&problem.Error{Code: problem.RelationChanged, Message: "SENSITIVE"}, 409},
		{&problem.Error{Code: problem.CellTooLarge, Message: "SENSITIVE"}, 422},
		{&problem.Error{Code: problem.UnsupportedColumnType, Message: "SENSITIVE"}, 422},
		{&problem.Error{Code: problem.BodyTooLarge}, 413},
		{&problem.Error{Code: problem.UnsupportedMediaType}, 415},
		{&problem.Error{Code: problem.ValidationFailed}, 422},
		{fmt.Errorf("SENSITIVE: %w", &problem.Error{Code: problem.PlatformUnavailable}), 503},
		{context.DeadlineExceeded, 504},
		{&problem.Error{Code: "SENSITIVE", Message: "SENSITIVE"}, 500},
		{errors.New("postgres://SENSITIVE"), 500},
	} {
		status, code, message := ErrorInfo(tt.err)
		if status != tt.status || strings.Contains(string(code)+message, "SENSITIVE") {
			t.Fatalf("unsafe or incorrect error mapping: %d %s %s", status, code, message)
		}
	}
}

func TestWriteErrorRedirectsInvalidSession(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/users", nil)
	WriteError(w, r, fmt.Errorf("wrapped: %w", &problem.Error{Code: problem.SessionInvalid}))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("session redirect = %d %v", w.Code, w.Header())
	}
}

func TestSeeOtherInternalPaths(t *testing.T) {
	for _, location := range []string{"/databases", "/drafts/review?" + url.Values{"schema": {"猫 & public"}}.Encode()} {
		w := httptest.NewRecorder()
		if err := SeeOther(w, httptest.NewRequest(http.MethodPost, "/", nil), location); err != nil {
			t.Fatal(err)
		}
		if w.Code != 303 || w.Header().Get("Location") != location || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("redirect = %d %v", w.Code, w.Header())
		}
	}
	for _, location := range []string{"https://evil.example", "//evil.example", "login", `/\evil.example`, "/%2fevil.example", "/%5cevil.example", "/%0d%0aLocation:evil", "/login#fragment"} {
		w := httptest.NewRecorder()
		err := SeeOther(w, httptest.NewRequest(http.MethodPost, "/", nil), location)
		requireCode(t, err, problem.InternalError)
		if w.Header().Get("Location") != "" || w.Body.Len() != 0 {
			t.Fatalf("unsafe redirect written for %q", location)
		}
	}
}
