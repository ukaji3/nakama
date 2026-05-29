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
