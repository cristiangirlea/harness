package owner

import "testing"

func TestLocalUsesValidOverride(t *testing.T) {
	t.Setenv("HRN_OWNER", "user:alice")
	if got := Local(); got != "user:alice" {
		t.Fatalf("Local() = %q, want user:alice", got)
	}
	t.Setenv("HRN_OWNER", "nonsense")
	if got := Local(); got == "nonsense" || !Valid(got) || got[:len(LocalPrefix)] != LocalPrefix {
		t.Fatalf("an invalid HRN_OWNER must fall back to the OS user, got %q", got)
	}
}

func TestValid(t *testing.T) {
	for id, want := range map[string]bool{
		"local:bob":         true,
		"email:a@b.example": true,
		"user:42":           true,
		"":                  false,
		"bob":               false,
		"root:bob":          false,
		"local:":            false,
		"local:a\nb":        false,
	} {
		if Valid(id) != want {
			t.Errorf("Valid(%q) = %v, want %v", id, !want, want)
		}
	}
}

func TestLegacyRecordsBelongToTheLocalUser(t *testing.T) {
	t.Setenv("HRN_OWNER", "local:me")
	if Of("") != "local:me" {
		t.Fatalf("Of(\"\") = %q", Of(""))
	}
	if !LocalViewer().CanRead("") {
		t.Fatal("the local user must read records written before ownership")
	}
}

func TestCanRead(t *testing.T) {
	t.Setenv("HRN_OWNER", "local:me")
	alice, bob := Viewer{ID: Email("Alice@Example.com ")}, Viewer{ID: Email("bob@example.com")}
	if !alice.CanRead("email:alice@example.com") {
		t.Fatal("an owner must read their own record")
	}
	if bob.CanRead("email:alice@example.com") || bob.CanRead("") {
		t.Fatal("another user must not read a private or a local record")
	}
	if (Viewer{}).CanRead("") {
		t.Fatal("an anonymous viewer must read nothing")
	}
	if !(Viewer{Admin: true}).CanRead("email:alice@example.com") {
		t.Fatal("an admin grant reads every record")
	}
}
