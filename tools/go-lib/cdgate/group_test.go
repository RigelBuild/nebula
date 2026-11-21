package cdgate

import (
	"errors"
	"syscall"
	"testing"
)

func failWith(errno syscall.Errno) Kill {
	return func(int, syscall.Signal) error { return errno }
}

func noList(t *testing.T) ListProcesses {
	t.Helper()
	return func() (string, error) {
		t.Fatal("process list should not run")
		return "", nil
	}
}

func listOf(table string) ListProcesses {
	return func() (string, error) { return table, nil }
}

func TestEPERMWithOnlyZombieMembersIsFalse(t *testing.T) {
	got, err := SignalGroup(42, syscall.SIGTERM, failWith(syscall.EPERM),
		listOf("   42 Z\n   42 Z+\n   43 S\n"))
	if err != nil || got {
		t.Fatalf("SignalGroup = %v, %v; want false, nil", got, err)
	}
}

func TestEPERMWithLiveMemberIsTrue(t *testing.T) {
	got, err := SignalGroup(42, syscall.SIGTERM, failWith(syscall.EPERM),
		listOf("   42 Z\n   42 S\n   43 R\n"))
	if err != nil || !got {
		t.Fatalf("SignalGroup = %v, %v; want true, nil", got, err)
	}
}

func TestESRCHIsFalseWithoutListing(t *testing.T) {
	got, err := SignalGroup(42, syscall.SIGTERM, failWith(syscall.ESRCH), noList(t))
	if err != nil || got {
		t.Fatalf("SignalGroup = %v, %v; want false, nil", got, err)
	}
}

func TestOtherSignalErrorsAreReturned(t *testing.T) {
	_, err := SignalGroup(42, syscall.SIGTERM, failWith(syscall.EACCES), noList(t))
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
}

func TestSuccessfulSignalIsTrue(t *testing.T) {
	got, err := SignalGroup(42, syscall.SIGTERM,
		func(int, syscall.Signal) error { return nil }, noList(t))
	if err != nil || !got {
		t.Fatalf("SignalGroup = %v, %v; want true, nil", got, err)
	}
}
