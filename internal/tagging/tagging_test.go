package tagging

import "testing"

func TestForInstance(t *testing.T) {
	t.Parallel()
	got := ForInstance("dev01", ComponentEC2, "nakama-dev01")
	if got[KeyManaged] != ValueManagedTrue {
		t.Errorf("KeyManaged=%q, want %q", got[KeyManaged], ValueManagedTrue)
	}
	if got[KeyName] != "dev01" {
		t.Errorf("KeyName=%q, want dev01", got[KeyName])
	}
	if got[KeyComponent] != ComponentEC2 {
		t.Errorf("KeyComponent=%q, want %q", got[KeyComponent], ComponentEC2)
	}
	if got[NameTag] != "nakama-dev01" {
		t.Errorf("NameTag=%q, want nakama-dev01", got[NameTag])
	}
}

func TestForShared(t *testing.T) {
	t.Parallel()
	got := ForShared(ComponentVPC, "nakama-vpc")
	if got[KeyManaged] != ValueManagedTrue {
		t.Errorf("KeyManaged=%q, want %q", got[KeyManaged], ValueManagedTrue)
	}
	if _, ok := got[KeyName]; ok {
		t.Errorf("ForShared should not set KeyName, got %q", got[KeyName])
	}
	if got[KeyComponent] != ComponentVPC {
		t.Errorf("KeyComponent=%q, want %q", got[KeyComponent], ComponentVPC)
	}
	if got[NameTag] != "nakama-vpc" {
		t.Errorf("NameTag=%q, want nakama-vpc", got[NameTag])
	}
}

func TestBase(t *testing.T) {
	t.Parallel()
	got := Base()
	if len(got) != 1 {
		t.Errorf("Base() should return exactly 1 entry, got %d", len(got))
	}
	if got[KeyManaged] != ValueManagedTrue {
		t.Errorf("Base()[%q]=%q, want %q", KeyManaged, got[KeyManaged], ValueManagedTrue)
	}
}

func TestForBackup(t *testing.T) {
	t.Parallel()
	got := ForBackup("dev01", "before-x", "nakama-backup-dev01-before-x", "2026-05-29T00:00:00Z", "note")
	if got[KeyComponent] != ComponentBackup {
		t.Errorf("KeyComponent=%q, want %q", got[KeyComponent], ComponentBackup)
	}
	if got[KeyBackupLabel] != "before-x" || got[KeyBackupTime] != "2026-05-29T00:00:00Z" {
		t.Errorf("label/time tags wrong: %v", got)
	}
	if got[KeyNote] != "note" {
		t.Errorf("KeyNote=%q, want note", got[KeyNote])
	}
	// Empty note must not set the note tag.
	if _, ok := ForBackup("dev01", "l", "n", "t", "")[KeyNote]; ok {
		t.Error("empty note should omit KeyNote")
	}
}
