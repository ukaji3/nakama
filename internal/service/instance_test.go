package service

import (
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/config"
)

func TestValidateInstanceName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"dev01", false},
		{"a1", false},
		{"a-b-c", false},
		{"abcdefghijklmnopqrstuvwxyz0123ab", false}, // 32 chars
		{"abcdefghijklmnopqrstuvwxyz0123abc", true}, // 33 chars
		{"a", true},      // too short
		{"-abc", true},   // leading hyphen
		{"abc-", true},   // trailing hyphen
		{"Dev01", true},  // uppercase
		{"dev_01", true}, // underscore
		{"", true},
	}
	for _, c := range cases {
		err := ValidateInstanceName(c.in)
		got := err != nil
		if got != c.wantErr {
			t.Errorf("ValidateInstanceName(%q) wantErr=%v, gotErr=%v (err=%v)",
				c.in, c.wantErr, got, err)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	t.Parallel()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

	a, err := generatePassword()
	if err != nil {
		t.Fatalf("generatePassword: %v", err)
	}
	if len(a) != config.PasswordLength {
		t.Errorf("len=%d, want %d", len(a), config.PasswordLength)
	}
	for _, ch := range a {
		if !strings.ContainsRune(alphabet, ch) {
			t.Errorf("character %q not in allowed alphabet", ch)
		}
	}

	b, err := generatePassword()
	if err != nil {
		t.Fatalf("generatePassword: %v", err)
	}
	if a == b {
		t.Errorf("two consecutive passwords were identical: %q (entropy bug?)", a)
	}
}
