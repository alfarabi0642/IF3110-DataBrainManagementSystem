package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestResolutionValidation(t *testing.T) {
	valid := ResolutionInput{"succeeded", "Operator 猫", "Confirmed target commit from operator records.\r\n"}
	if err := ValidateResolution(valid); err != nil {
		t.Fatal(err)
	}
	for _, input := range []ResolutionInput{
		{"unknown", valid.Operator, valid.Evidence}, {"failed", "", valid.Evidence},
		{"failed", " \t", valid.Evidence}, {"failed", strings.Repeat("猫", 121), valid.Evidence},
		{"failed", "bad\x00name", valid.Evidence}, {"failed", string([]byte{0xff}), valid.Evidence},
		{"failed", valid.Operator, " \r\n"}, {"failed", valid.Operator, "bad\x00evidence"},
		{"failed", valid.Operator, string([]byte{0xff})}, {"failed", valid.Operator, strings.Repeat("x", MaxResolutionBytes+1)},
	} {
		if err := ResolveAttempt(context.Background(), nil, uuid.New(), input); !errors.Is(err, ErrResolutionInput) {
			t.Fatal("invalid resolution reached database or leaked input")
		}
	}
	if err := ResolveAttempt(context.Background(), nil, uuid.Nil, valid); !errors.Is(err, ErrResolutionInput) {
		t.Fatal("nil attempt accepted")
	}
	if err := ValidateResolution(ResolutionInput{"failed", "Operator", strings.Repeat("x", MaxResolutionBytes)}); err != nil {
		t.Fatal("maximum evidence size rejected")
	}
}
