package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		valid    bool
	}{
		{"empty", "", false},
		{"11 bytes", strings.Repeat("a", 11), false},
		{"12 bytes", strings.Repeat("a", 12), true},
		{"128 bytes", strings.Repeat("a", 128), true},
		{"129 bytes", strings.Repeat("a", 129), false},
		{"four cats are 12 bytes", "猫猫猫猫", true},
		{"unicode over byte limit", strings.Repeat("猫", 43), false},
		{"whitespace is preserved", "            ", true},
		{"invalid UTF-8", strings.Repeat("a", 12) + "\xff", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrInvalidPassword) {
				t.Fatalf("ValidatePassword error = %v, valid = %v", err, tc.valid)
			}
			if !tc.valid {
				if hash, err := HashPassword(context.Background(), tc.password); hash != "" || !errors.Is(err, ErrInvalidPassword) {
					t.Fatal("HashPassword accepted invalid input")
				}
				if match, err := VerifyPassword(context.Background(), tc.password, dummyPasswordHash); match || !errors.Is(err, ErrInvalidPassword) {
					t.Fatal("VerifyPassword accepted invalid input")
				}
				if err := CheckDummyPassword(context.Background(), tc.password); !errors.Is(err, ErrInvalidPassword) {
					t.Fatal("CheckDummyPassword accepted invalid input")
				}
			}
		})
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	ctx := context.Background()
	password := " 猫猫猫猫 "
	first, err := HashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("hashes reused a salt")
	}
	for _, encoded := range []string{first, second} {
		parts := strings.Split(encoded, "$")
		if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=2" {
			t.Fatal("hash does not use the required PHC policy")
		}
		salt, saltErr := base64.RawStdEncoding.DecodeString(parts[4])
		key, keyErr := base64.RawStdEncoding.DecodeString(parts[5])
		if saltErr != nil || keyErr != nil || len(salt) != 16 || len(key) != 32 {
			t.Fatal("incorrect salt or key encoding/length")
		}
		match, err := VerifyPassword(ctx, password, encoded)
		if err != nil || !match {
			t.Fatalf("correct password: match=%v, err=%v", match, err)
		}
	}
	for _, wrong := range []string{"猫猫猫猫", "Other-valid-password"} {
		match, err := VerifyPassword(ctx, wrong, first)
		if err != nil || match {
			t.Fatalf("changed password: match=%v, err=%v", match, err)
		}
	}
}

func TestVerifyPasswordDoesNotNormalizeUnicode(t *testing.T) {
	password := strings.Repeat("é", 6)
	encoded, err := HashPassword(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	match, err := VerifyPassword(context.Background(), strings.Repeat("e\u0301", 6), encoded)
	if err != nil || match {
		t.Fatalf("normalization changed password identity: match=%v, err=%v", match, err)
	}
}

func TestDummyPassword(t *testing.T) {
	match, err := VerifyPassword(context.Background(), "DataBrain-dummy-password", dummyPasswordHash)
	if err != nil || !match {
		t.Fatalf("dummy fixture is not a valid policy hash: match=%v, err=%v", match, err)
	}
	for _, password := range []string{"DataBrain-dummy-password", "Different-valid-password"} {
		if err := CheckDummyPassword(context.Background(), password); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRejectMalformedPasswordHashes(t *testing.T) {
	cases := map[string]string{
		"empty":                  "",
		"huge input":             strings.Repeat("x", 1<<20),
		"wrong algorithm":        strings.Replace(dummyPasswordHash, "argon2id", "argon2i", 1),
		"wrong version":          strings.Replace(dummyPasswordHash, "v=19", "v=16", 1),
		"excessive memory":       strings.Replace(dummyPasswordHash, "65536", "99999", 1),
		"excessive iterations":   strings.Replace(dummyPasswordHash, "t=3", "t=9", 1),
		"zero parallelism":       strings.Replace(dummyPasswordHash, "p=2", "p=0", 1),
		"missing separator":      strings.Replace(dummyPasswordHash, "WMQ$", "WMQx", 1),
		"extra field":            dummyPasswordHash + "$extra",
		"padding":                dummyPasswordHash + "=",
		"short key":              dummyPasswordHash[:len(dummyPasswordHash)-1],
		"invalid salt alphabet":  strings.Replace(dummyPasswordHash, "RGF0", "!GF0", 1),
		"invalid key alphabet":   strings.Replace(dummyPasswordHash, "mfe1", "!fe1", 1),
		"noncanonical salt bits": strings.Replace(dummyPasswordHash, "WMQ$", "WMR$", 1),
		"noncanonical key bits":  dummyPasswordHash[:len(dummyPasswordHash)-1] + "x",
		"embedded newline":       strings.Replace(dummyPasswordHash, "RGF0", "RG\n0", 1),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			// Hash rusak harus ditolak sebelum jalur hashing dan pembatasan kapasitas.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			match, err := VerifyPassword(ctx, "Valid-test-password", encoded)
			if match || !errors.Is(err, ErrInvalidPasswordHash) {
				t.Fatalf("match=%v, err=%v", match, err)
			}
		})
	}
}

func passwordOperations() map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"hash": func(ctx context.Context) error {
			_, err := HashPassword(ctx, "Valid-test-password")
			return err
		},
		"verify": func(ctx context.Context) error {
			_, err := VerifyPassword(ctx, "Valid-test-password", dummyPasswordHash)
			return err
		},
		"dummy": func(ctx context.Context) error {
			return CheckDummyPassword(ctx, "Valid-test-password")
		},
	}
}

func TestPasswordCapacityAndCancellation(t *testing.T) {
	for name, operation := range passwordOperations() {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Channel kapasitas berada dalam bubble test agar penantiannya memajukan waktu simulasi.
				originalSlots := passwordHashSlots
				passwordHashSlots = make(chan struct{}, 2)
				defer func() { passwordHashSlots = originalSlots }()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := operation(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("already canceled: %v", err)
				}
				if cap(passwordHashSlots) != 2 || len(passwordHashSlots) != 0 {
					t.Fatal("expected two free hashing slots")
				}
				passwordHashSlots <- struct{}{}
				passwordHashSlots <- struct{}{}
				defer func() { <-passwordHashSlots; <-passwordHashSlots }()
				start := time.Now()
				if err := operation(context.Background()); !errors.Is(err, ErrPasswordHashBusy) {
					t.Fatalf("capacity error = %v", err)
				}
				if elapsed := time.Since(start); elapsed != 2*time.Second {
					t.Fatalf("capacity wait = %v", elapsed)
				}
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := operation(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("waiting cancellation = %v", err)
				}
				if len(passwordHashSlots) != 2 {
					t.Fatal("waiting request changed occupied slots")
				}
			})
		})
	}
}

func TestPasswordOperationsShareCapacity(t *testing.T) {
	var wg sync.WaitGroup
	for name, operation := range passwordOperations() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := operation(context.Background()); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}()
	}
	wg.Wait()
	if len(passwordHashSlots) != 0 {
		t.Fatal("hashing slots leaked")
	}
}
