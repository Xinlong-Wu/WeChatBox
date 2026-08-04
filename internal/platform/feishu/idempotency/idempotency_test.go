package idempotency

import "testing"

func TestStableUUIDIsDeterministicAndNamespaced(t *testing.T) {
	first := StableUUID("workflow-message", "req_123", "0")
	if first != StableUUID("workflow-message", "req_123", "0") {
		t.Fatal("StableUUID changed for the same namespace and inputs")
	}
	if first == StableUUID("docx-append", "req_123", "0") {
		t.Fatal("StableUUID reused an identifier across namespaces")
	}
	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("StableUUID = %q, want RFC 4122-shaped value", first)
	}
}
