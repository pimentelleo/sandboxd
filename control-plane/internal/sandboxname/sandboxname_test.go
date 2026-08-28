package sandboxname

import "testing"

func TestContainerUsesLowercaseID(t *testing.T) {
	if got, want := Container("01M14B2Z6T97D5ZBCQRVZ1463E"), "s-01m14b2z6t97d5zbcqrvz1463e"; got != want {
		t.Fatalf("Container() = %q, want %q", got, want)
	}
}

func TestReferencePrefersPersistedContainerID(t *testing.T) {
	if got, want := Reference("01M14B2Z6T97D5ZBCQRVZ1463E", "c6097a82c9fd"), "c6097a82c9fd"; got != want {
		t.Fatalf("Reference() = %q, want %q", got, want)
	}
}

func TestIDFromContainerRestoresCanonicalID(t *testing.T) {
	id, ok := IDFromContainer("s-01m14b2z6t97d5zbcqrvz1463e")
	if !ok || id != "01M14B2Z6T97D5ZBCQRVZ1463E" {
		t.Fatalf("IDFromContainer() = %q, %t", id, ok)
	}
	if _, ok := IDFromContainer("sandboxd-child-01m14b2z6t97d5zbcqrvz1463e"); ok {
		t.Fatal("worker name was accepted as a sandbox name")
	}
}
