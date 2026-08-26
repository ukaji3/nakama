package signin

import (
	"encoding/base64"
	"testing"
)

func TestParseVerificationCode_OK(t *testing.T) {
	t.Parallel()
	raw := base64.StdEncoding.EncodeToString([]byte("state=st-123&code=co-456"))
	code, state, err := ParseVerificationCode(raw)
	if err != nil {
		t.Fatalf("ParseVerificationCode: %v", err)
	}
	if code != "co-456" || state != "st-123" {
		t.Errorf("(code,state)=(%q,%q)", code, state)
	}
}

func TestParseVerificationCode_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	raw := "  " + base64.StdEncoding.EncodeToString([]byte("state=s&code=c")) + "\n"
	code, state, err := ParseVerificationCode(raw)
	if err != nil {
		t.Fatalf("ParseVerificationCode: %v", err)
	}
	if code != "c" || state != "s" {
		t.Errorf("(code,state)=(%q,%q)", code, state)
	}
}

func TestParseVerificationCode_InvalidBase64(t *testing.T) {
	t.Parallel()
	if _, _, err := ParseVerificationCode("!!!not-base64!!!"); err == nil {
		t.Error("expected decode error")
	}
}

func TestParseVerificationCode_MissingKeys(t *testing.T) {
	t.Parallel()
	cases := []string{
		"state=only-state",
		"code=only-code",
		"foo=bar",
		"",
	}
	for _, qs := range cases {
		raw := base64.StdEncoding.EncodeToString([]byte(qs))
		if _, _, err := ParseVerificationCode(raw); err == nil {
			t.Errorf("query %q: expected error", qs)
		}
	}
}
