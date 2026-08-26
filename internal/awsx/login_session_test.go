package awsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginSessionFromConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		profile string
		want    string
	}{
		{
			name: "default section",
			in: "[default]\n" +
				"login_session = arn:aws:signin:::user/example\n" +
				"region = ap-northeast-1\n",
			want: "arn:aws:signin:::user/example",
		},
		{
			name: "other profile sections are ignored",
			in: "[profile work]\n" +
				"login_session = arn:aws:signin:::user/work\n" +
				"[default]\n" +
				"login_session = arn:aws:signin:::user/default\n" +
				"[profile play]\n" +
				"login_session = arn:aws:signin:::user/play\n",
			want: "arn:aws:signin:::user/default",
		},
		{
			name: "no default section",
			in: "[profile work]\n" +
				"login_session = arn:aws:signin:::user/work\n",
			want: "",
		},
		{
			name: "key missing in default",
			in: "[default]\n" +
				"region = ap-northeast-1\n",
			want: "",
		},
		{
			name: "full-line comments and blank lines",
			in: "# leading comment\n" +
				"; another comment style\n" +
				"\n" +
				"[default]\n" +
				"# login_session = arn:aws:signin:::user/commented-out\n" +
				"; login_session = arn:aws:signin:::user/also-commented\n" +
				"login_session = arn:aws:signin:::user/real\n",
			want: "arn:aws:signin:::user/real",
		},
		{
			name: "whitespace and case tolerance",
			in: "  [ DEFAULT ]  \n" +
				"   Login_Session   =    arn:aws:signin:::user/spacey   \n",
			want: "arn:aws:signin:::user/spacey",
		},
		{
			name: "value keeps inner content verbatim",
			in: "[default]\n" +
				"login_session=arn:aws:signin:::user/tight\n",
			want: "arn:aws:signin:::user/tight",
		},
		{
			name: "malformed lines are skipped",
			in: "[default]\n" +
				"not-a-key-value-line\n" +
				"login_session = arn:aws:signin:::user/after-noise\n",
			want: "arn:aws:signin:::user/after-noise",
		},
		{
			name: "empty document",
			in:   "",
			want: "",
		},
		{
			name: "named profile section",
			in: "[default]\n" +
				"login_session = arn:aws:signin:::user/default\n" +
				"[profile work]\n" +
				"login_session = arn:aws:signin:::user/work\n",
			profile: "work",
			want:    "arn:aws:signin:::user/work",
		},
		{
			name: "profile named default aliases the default section",
			in: "[default]\n" +
				"login_session = arn:aws:signin:::user/default\n",
			profile: "default",
			want:    "arn:aws:signin:::user/default",
		},
		{
			name: "absent named section has no fallback",
			in: "[default]\n" +
				"login_session = arn:aws:signin:::user/default\n",
			profile: "absent",
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := loginSessionFromConfig(strings.NewReader(tc.in), tc.profile); got != tc.want {
				t.Errorf("loginSessionFromConfig=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveLoginSession_AWSConfigFileEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom_config")
	content := "[default]\nlogin_session = arn:aws:signin:::user/from-env\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AWS_CONFIG_FILE", path)

	got, err := ResolveLoginSession("")
	if err != nil {
		t.Fatalf("ResolveLoginSession: %v", err)
	}
	if got != "arn:aws:signin:::user/from-env" {
		t.Errorf("session=%q", got)
	}
}

func TestResolveLoginSession_ProfileSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	content := "[default]\n" +
		"login_session = arn:aws:signin:::user/default\n" +
		"[profile work]\n" +
		"login_session = arn:aws:signin:::user/work\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AWS_CONFIG_FILE", path)

	got, err := ResolveLoginSession("work")
	if err != nil {
		t.Fatalf("ResolveLoginSession(work): %v", err)
	}
	if got != "arn:aws:signin:::user/work" {
		t.Errorf("session=%q, want the [profile work] value", got)
	}

	// AWS_PROFILE routing via ResolveCurrentLoginSession.
	t.Setenv("AWS_PROFILE", "work")
	got, err = ResolveCurrentLoginSession()
	if err != nil {
		t.Fatalf("ResolveCurrentLoginSession: %v", err)
	}
	if got != "arn:aws:signin:::user/work" {
		t.Errorf("session=%q, want the AWS_PROFILE section value", got)
	}

	// An absent named section yields empty (no cross-section fallback).
	got, err = ResolveLoginSession("absent")
	if err != nil {
		t.Fatalf("ResolveLoginSession(absent): %v", err)
	}
	if got != "" {
		t.Errorf("session=%q, want empty for an absent profile section", got)
	}
}

func TestResolveLoginSession_MissingFileIsEmpty(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	got, err := ResolveLoginSession("")
	if err != nil {
		t.Fatalf("missing file must not be an error, got %v", err)
	}
	if got != "" {
		t.Errorf("session=%q, want empty", got)
	}
}
