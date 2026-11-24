package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

// fakeBackupDeleteBackend serves one backup and records deletions.
type fakeBackupDeleteBackend struct {
	backup  *service.Backup
	findErr error
	deleted []*service.Backup
}

func (f *fakeBackupDeleteBackend) FindBackup(context.Context, string, string) (*service.Backup, error) {
	return f.backup, f.findErr
}

func (f *fakeBackupDeleteBackend) DeleteFoundBackup(_ context.Context, b *service.Backup) error {
	f.deleted = append(f.deleted, b)
	return nil
}

// runBackupDelete runs `nakama aws backup delete <args>` with the fake
// and the given answer (or error) to the prompt.
func runBackupDelete(t *testing.T, b *fakeBackupDeleteBackend, answer bool, answerErr error, args ...string) (string, bool, error) {
	t.Helper()
	asked := false
	cmd := newAWSBackupDeleteCommandWithDeps(backupDeleteDeps{
		connect: func(context.Context) (backupDeleteBackend, error) { return b, nil },
		confirm: func() (bool, error) { asked = true; return answer, answerErr },
	})
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), asked, err
}

// TestBackupDelete_AsksFirst: like the other deletes, `backup delete`
// shows what it removes and asks; --yes / -y skip the question (Issue
// #129). Not parallel: the output is rendered with global.Output,
// which NewRootCommand in parallel tests writes.
func TestBackupDelete_AsksFirst(t *testing.T) {
	bk := &service.Backup{Name: "dev01", Label: "pre-upgrade", Note: "before 26.04", CreatedAt: "2026-11-01T00:00:00Z",
		SizeGiB: 30, SnapshotID: "snap-0123"}
	b := &fakeBackupDeleteBackend{backup: bk}

	out, asked, err := runBackupDelete(t, b, false, nil, "dev01", "--label", "pre-upgrade")
	if !asked || err == nil || err.Error() != "aborted" || len(b.deleted) != 0 {
		t.Fatalf("declined: asked %v, err %v, deleted %d", asked, err, len(b.deleted))
	}
	for _, want := range []string{
		`Delete backup "pre-upgrade" of "dev01"?`, "snap-0123 (30 GiB, created 2026-11-01T00:00:00Z)",
		"note       : before 26.04", "cannot be restored from", "[y/N]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q:\n%s", want, out)
		}
	}

	out, asked, err = runBackupDelete(t, b, true, nil, "dev01", "--label", "pre-upgrade")
	if !asked || err != nil || len(b.deleted) != 1 || b.deleted[0] != bk || !strings.Contains(out, "backup deleted: pre-upgrade") {
		t.Fatalf("accepted: asked %v, err %v, deleted %d, out %q", asked, err, len(b.deleted), out)
	}

	for _, yes := range []string{"--yes", "-y"} {
		_, asked, err = runBackupDelete(t, b, false, nil, "dev01", "--label", "pre-upgrade", yes)
		if asked || err != nil {
			t.Errorf("%s: asked %v, err %v", yes, asked, err)
		}
	}
	if len(b.deleted) != 3 {
		t.Errorf("deleted %d times, want 3", len(b.deleted))
	}

	// No answer (a script without --yes: stdin at EOF) deletes nothing.
	_, _, err = runBackupDelete(t, b, false, errors.New("EOF"), "dev01", "--label", "pre-upgrade")
	if err == nil || len(b.deleted) != 3 {
		t.Errorf("no answer: err %v, deleted %d", err, len(b.deleted))
	}
}

// TestBackupDelete_UnknownLabelDoesNotAsk: a label that does not exist
// fails before the prompt.
func TestBackupDelete_UnknownLabelDoesNotAsk(t *testing.T) {
	b := &fakeBackupDeleteBackend{findErr: errors.New(`backup label "nope" not found for instance "dev01"`)}
	out, asked, err := runBackupDelete(t, b, true, nil, "dev01", "--label", "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") || asked || len(b.deleted) != 0 || out != "" {
		t.Errorf("err %v, asked %v, deleted %d, out %q", err, asked, len(b.deleted), out)
	}
}

// TestBackupDeletePrompt_WithoutNote: the note line appears only when
// the backup has one.
func TestBackupDeletePrompt_WithoutNote(t *testing.T) {
	t.Parallel()
	p := backupDeletePrompt("dev01", &service.Backup{Label: "l1", SnapshotID: "snap-1", SizeGiB: 8, CreatedAt: "2026-11-02T00:00:00Z"})
	if strings.Contains(p, "note") {
		t.Errorf("prompt shows an empty note:\n%s", p)
	}
}
