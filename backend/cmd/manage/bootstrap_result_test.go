package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
)

func TestBootstrapResultOnlyExistingAdminIsIdempotent(t *testing.T) {
	for _, tt := range []struct {
		name             string
		bootstrap, close error
		code             int
	}{
		{"created", nil, nil, 0},
		{"existing", auth.ErrBootstrapAdminExists, nil, 0},
		{"invalid password", auth.ErrInvalidPassword, nil, 1},
		{"database failure", auth.ErrBootstrapFailed, nil, 1},
		{"connection failure", platformdb.ErrUnavailable, nil, 1},
		{"cleanup failure", auth.ErrBootstrapAdminExists, errors.New("SENSITIVE"), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := bootstrapResult(tt.bootstrap, tt.close, &stdout, &stderr); got != tt.code {
				t.Fatalf("exit=%d want=%d", got, tt.code)
			}
			if tt.code != 0 && (stdout.Len() != 0 || stderr.Len() == 0) {
				t.Fatal("failure reported as success")
			}
			if bytes.Contains(append(stdout.Bytes(), stderr.Bytes()...), []byte("SENSITIVE")) {
				t.Fatal("driver detail disclosed")
			}
		})
	}
}
