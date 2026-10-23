package access

import (
	"testing"

	"github.com/google/uuid"
)

func TestAuthorizerRejectsMissingDependenciesAndPrincipal(t *testing.T) {
	if authorizer, err := NewAuthorizer(nil); authorizer != nil || err == nil {
		t.Fatal("nil platform pool accepted")
	}
	if validPrincipal(Principal{}) {
		t.Fatal("empty principal accepted")
	}
	if !validPrincipal(Principal{UserID: uuid.New(), SessionID: uuid.New(), SecurityVersion: 1}) {
		t.Fatal("complete principal rejected")
	}
}
