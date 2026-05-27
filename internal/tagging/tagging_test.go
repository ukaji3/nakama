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
