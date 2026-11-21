package seat

import (
	"context"
	"testing"
)

func TestProcessIDAcceptsPositiveDecimalsOnly(t *testing.T) {
	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{value: "789\n", want: "789", ok: true},
		{value: " 42 ", want: "42", ok: true},
		{value: "0", want: "", ok: false},
		{value: "00", want: "", ok: false},
		{value: "", want: "", ok: false},
		{value: "12x", want: "", ok: false},
	}
	for _, test := range tests {
		got, ok := processID(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("processID(%q) = %q, %v; want %q, %v", test.value, got, ok, test.want, test.ok)
		}
	}
}

func TestSeatConfigForMapsBinDirectoryToShareDirectory(t *testing.T) {
	// The nix install puts the binary in <prefix>/bin and seat.conf in <prefix>/share/wave-seat-holder.
	tests := []struct {
		exe  string
		want string
	}{
		{exe: "/nix/store/abc-wave/bin/wave-seat-holder", want: "/nix/store/abc-wave/share/wave-seat-holder/seat.conf"},
		{exe: "/usr/local/bin/wave-seat-holder", want: "/usr/local/share/wave-seat-holder/seat.conf"},
	}
	for _, test := range tests {
		assertDeepEqual(t, seatConfigFor(test.exe), test.want)
	}
}

func TestSpawnCommandReturnsTheExitCodeOfACompletedChild(t *testing.T) {
	result, err := spawnCommand(context.Background(), "sh", []string{"-c", "exit 3"}, false)
	if err != nil {
		t.Fatalf("spawnCommand: %v", err)
	}
	assertDeepEqual(t, result.Code, 3)
}

func TestSpawnCommandReportsAChildThatCannotStart(t *testing.T) {
	_, err := spawnCommand(context.Background(), "wave-seat-holder-missing-binary", nil, false)
	if err == nil {
		t.Fatal("spawnCommand of a missing binary returned a nil error")
	}
}
