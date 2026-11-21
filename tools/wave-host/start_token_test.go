package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSameStartTokenAcceptsLegacyDottedRecord(t *testing.T) {
	// A record written before the integer token shipped holds sec.usec.
	// It must still match the live process, or a live seat reads as reused.
	if !sameStartToken("1700000000.123456", "1700000000123456") {
		t.Fatal("legacy dotted record did not match the same process")
	}
}

func TestSameStartTokenRejectsDifferentProcess(t *testing.T) {
	if sameStartToken("1700000000.123456", "1700000000123457") {
		t.Fatal("legacy record matched a different start time")
	}
	if sameStartToken("1700000000123456", "1700000000123457") {
		t.Fatal("integer record matched a different start time")
	}
}

func TestSameStartTokenUnparseableIsNotAMatch(t *testing.T) {
	for _, recorded := range []string{"", "garbage", "1700000000.12"} {
		if sameStartToken(recorded, "1700000000123456") {
			t.Fatalf("unparseable recorded token %q matched", recorded)
		}
	}
}

func TestSameStartTokenIntegerRecordMatchesItself(t *testing.T) {
	if !sameStartToken("1700000000123456", "1700000000123456") {
		t.Fatal("integer record did not match itself")
	}
}

func TestIsMalformedDottedTokenFlagsOnlyBrokenDottedRecords(t *testing.T) {
	for _, token := range []string{"1700000000.12", "1700000000.1234567", "x.123456", "1700000000.abcdef"} {
		if !isMalformedDottedToken(token) {
			t.Fatalf("malformed dotted token %q not flagged", token)
		}
	}
	for _, token := range []string{"1700000000.123456", "1700000000123456", "boot:123", "tok"} {
		if isMalformedDottedToken(token) {
			t.Fatalf("token %q wrongly flagged malformed", token)
		}
	}
}

func TestRecordedGoneRefusesUnreadableRecordedToken(t *testing.T) {
	// An unreadable recorded token must not read as a reused, gone process.
	h := seatHost{identify: func(pid int) (Identity, error) {
		return Identity{PID: pid, StartToken: "1700000000123456"}, nil
	}}
	err := h.recordedGone(seatRecord{HolderPID: 11, HolderToken: "1700000000.12", AgentPID: 22, AgentToken: "1700000000123456"})
	if err == nil {
		t.Fatal("an unreadable recorded token was treated as gone")
	}
}

// A Linux token is boot_id:starttime. It must match itself and nothing else.
func TestSameStartTokenLinuxShapeMatchesItself(t *testing.T) {
	if !sameStartToken("boot:123", "boot:123") {
		t.Fatal("linux-shaped token did not match itself")
	}
	if sameStartToken("boot:123", "boot:124") {
		t.Fatal("linux-shaped token matched a different starttime")
	}
}

// An opaque token is compared as an exact string. The helper must not parse it.
func TestSameStartTokenOpaqueIsExactString(t *testing.T) {
	if !sameStartToken("tok", "tok") {
		t.Fatal("opaque token did not match itself")
	}
	if sameStartToken("tok", "tok2") {
		t.Fatal("opaque token matched a different value")
	}
}

func TestNormalizeStartTokenConvertsOnlyLegacyDotted(t *testing.T) {
	if got := normalizeStartToken("1700000000.123456"); got != "1700000000123456" {
		t.Fatalf("legacy dotted record normalized to %q", got)
	}
	for _, token := range []string{"boot:123", "tok", "1700000000123456"} {
		if got := normalizeStartToken(token); got != token {
			t.Fatalf("non-legacy token %q changed to %q", token, got)
		}
	}
}

// The alive path reads a live process through the real comparison. A legacy
// record must match the live integer token, or a live manager reads as gone.
func TestManagerAliveMatchesLegacyRecordAgainstLiveProcess(t *testing.T) {
	m := managerHost{identify: func(pid int) (Identity, error) {
		return Identity{PID: pid, StartToken: "1700000000123456"}, nil
	}}
	live, err := m.alive(42, "1700000000.123456")
	if err != nil || !live {
		t.Fatalf("legacy record for a live manager: live=%v err=%v", live, err)
	}
}

// A malformed recorded token must be an error, never "not running". False
// would make start launch a second manager beside a live one.
func TestManagerAliveRefusesMalformedRecordedToken(t *testing.T) {
	m := managerHost{identify: func(pid int) (Identity, error) {
		return Identity{PID: pid, StartToken: "1700000000123456"}, nil
	}}
	for _, token := range []string{"1700000000.12", "1700000000.1234567", "x.123456"} {
		live, err := m.alive(42, token)
		if err == nil {
			t.Fatalf("malformed token %q: want error, got live=%v", token, live)
		}
	}
}

// A malformed token must stop start before the manager launches.
func TestManagerStartRefusedOnUnreadableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 42, ManagerToken: "1700000000.12"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login")
	launched := false
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		launched = true
		return Identity{}, nil, errors.New("launch must not run")
	}
	if err := m.start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("start accepted an unreadable recorded token")
	}
	if launched {
		t.Fatal("start launched a second manager beside a possibly live one")
	}
}
