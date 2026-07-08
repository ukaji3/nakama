package config

import (
	"strings"
	"testing"
)

func TestUbuntuAMISSMParameterFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arch    string
		want    string
		wantErr bool
	}{
		{arch: ArchX8664, want: UbuntuAMISSMParameterAMD64},
		{arch: ArchARM64, want: UbuntuAMISSMParameterARM64},
		{arch: "i386", wantErr: true},
		{arch: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			got, err := UbuntuAMISSMParameterFor(tt.arch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for arch %q, got %q", tt.arch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("param=%q, want %q", got, tt.want)
			}
		})
	}

	// Sanity: the two parameter paths differ only by the arch segment.
	if !strings.Contains(UbuntuAMISSMParameterAMD64, "/amd64/") {
		t.Errorf("amd64 param missing /amd64/ segment: %q", UbuntuAMISSMParameterAMD64)
	}
	if !strings.Contains(UbuntuAMISSMParameterARM64, "/arm64/") {
		t.Errorf("arm64 param missing /arm64/ segment: %q", UbuntuAMISSMParameterARM64)
	}
}
