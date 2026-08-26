package cli

import "testing"

func TestResolveConnectRetries(t *testing.T) {
	tests := []struct {
		name        string
		explicit    bool
		value       int
		commandMode bool
		want        int
	}{
		{name: "shell default", explicit: false, value: defaultConnectRetries, commandMode: false, want: 5},
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
