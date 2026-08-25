package cli

import "testing"

func TestParsePortForward(t *testing.T) {
	tests := []struct {
		name                       string
		spec                       string
		wantLocal, wantHost, wantR string
		wantErr                    bool
	}{
		{name: "instance L:R", spec: "8080:8080", wantLocal: "8080", wantHost: "", wantR: "8080"},
		{name: "remote host L:host:R", spec: "5432:db.internal:5433", wantLocal: "5432", wantHost: "db.internal", wantR: "5433"},
		{name: "too few parts", spec: "8080", wantErr: true},
		{name: "too many parts", spec: "1:2:3:4", wantErr: true},
		{name: "bad local port", spec: "0:8080", wantErr: true},
		{name: "bad remote port", spec: "8080:70000", wantErr: true},
		{name: "non-numeric port", spec: "abc:8080", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, host, remote, err := parsePortForward(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if local != tt.wantLocal || host != tt.wantHost || remote != tt.wantR {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					local, host, remote, tt.wantLocal, tt.wantHost, tt.wantR)
			}
		})
	}
}

func TestResolveConnectRetries(t *testing.T) {
	tests := []struct {
		name        string
		explicit    bool
		value       int
		commandMode bool
		want        int
	}{
		{name: "shell default", explicit: false, value: defaultConnectRetries, commandMode: false, want: 5},
		{name: "port-forward default (no command)", explicit: false, value: defaultConnectRetries, commandMode: false, want: 5},
		{name: "command default disables retries", explicit: false, value: defaultConnectRetries, commandMode: true, want: 0},
		{name: "explicit retry wins in command mode", explicit: true, value: 3, commandMode: true, want: 3},
		{name: "explicit retry wins in shell mode", explicit: true, value: 7, commandMode: false, want: 7},
		{name: "explicit zero disables in shell mode", explicit: true, value: 0, commandMode: false, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveConnectRetries(tt.explicit, tt.value, tt.commandMode); got != tt.want {
				t.Errorf("resolveConnectRetries(%v, %d, %v) = %d, want %d",
					tt.explicit, tt.value, tt.commandMode, got, tt.want)
			}
		})
	}
}
