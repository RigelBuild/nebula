package woodpecker

import "testing"

// NewDispatchID mints a fresh v4 UUID per call, the value a caller passes to
// DispatchCDTargets as PETREL_CD_DISPATCH_ID.
func TestNewDispatchIDIsV4UUIDAndUnique(t *testing.T) {
	first, err := NewDispatchID()
	if err != nil {
		t.Fatalf("NewDispatchID: %v", err)
	}
	second, err := NewDispatchID()
	if err != nil {
		t.Fatalf("NewDispatchID: %v", err)
	}
	if !dispatchNonce.MatchString(first) {
		t.Fatalf("NewDispatchID = %q, want a v4 UUID", first)
	}
	if first == second {
		t.Fatalf("two NewDispatchID calls returned the same id %q", first)
	}
}
