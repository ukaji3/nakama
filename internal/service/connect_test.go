package service

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestConnectDocument(t *testing.T) {
	tests := []struct {
		name      string
		in        ConnectInput
		wantDoc   *string
		wantParam map[string][]string
	}{
		{
			name:      "interactive shell",
			in:        ConnectInput{Name: "dev01"},
			wantDoc:   aws.String("AWS-StartInteractiveCommand"),
			wantParam: map[string][]string{"command": {"exec sudo su - ubuntu"}},
		},
		{
			name:      "command non-interactive",
			in:        ConnectInput{Name: "dev01", Command: []string{"ls", "-la"}},
			wantDoc:   aws.String("AWS-StartNonInteractiveCommand"),
			wantParam: map[string][]string{"command": {"sudo su - ubuntu -c 'ls -la'"}},
		},
		{
			name:      "command tty",
			in:        ConnectInput{Name: "dev01", Command: []string{"top"}, TTY: true},
			wantDoc:   aws.String("AWS-StartInteractiveCommand"),
			wantParam: map[string][]string{"command": {"sudo su - ubuntu -c 'top'"}},
		},
		{
			name:      "command with single quote is escaped",
			in:        ConnectInput{Name: "dev01", Command: []string{"echo", "it's"}},
			wantDoc:   aws.String("AWS-StartNonInteractiveCommand"),
			wantParam: map[string][]string{"command": {`sudo su - ubuntu -c 'echo it'\''s'`}},
		},
		{
			name:      "port forward to instance",
			in:        ConnectInput{Name: "dev01", LocalPort: "8080", RemotePort: "8080"},
			wantDoc:   aws.String("AWS-StartPortForwardingSession"),
			wantParam: map[string][]string{"portNumber": {"8080"}, "localPortNumber": {"8080"}},
		},
		{
			name:      "port forward to remote host",
			in:        ConnectInput{Name: "dev01", LocalPort: "5432", RemoteHost: "db.internal", RemotePort: "5432"},
			wantDoc:   aws.String("AWS-StartPortForwardingSessionToRemoteHost"),
			wantParam: map[string][]string{"portNumber": {"5432"}, "localPortNumber": {"5432"}, "host": {"db.internal"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDoc, gotParam := connectDocument(tt.in)
			if aws.ToString(gotDoc) != aws.ToString(tt.wantDoc) {
				t.Errorf("doc = %q, want %q", aws.ToString(gotDoc), aws.ToString(tt.wantDoc))
			}
			if len(gotParam) != len(tt.wantParam) {
				t.Fatalf("param = %v, want %v", gotParam, tt.wantParam)
			}
			for k, want := range tt.wantParam {
				got := gotParam[k]
				if len(got) != len(want) || (len(want) == 1 && got[0] != want[0]) {
					t.Errorf("param[%q] = %v, want %v", k, got, want)
				}
			}
		})
	}
}
