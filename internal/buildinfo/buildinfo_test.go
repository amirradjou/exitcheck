package buildinfo

import "testing"

func TestStringPrefersLinkTimeVersion(t *testing.T) {
	if got := String(); got == "" {
		t.Fatal("String() is empty")
	}
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v0.1.0"
	if got := String(); got != "v0.1.0" {
		t.Fatalf("String() = %q, want v0.1.0", got)
	}
}
