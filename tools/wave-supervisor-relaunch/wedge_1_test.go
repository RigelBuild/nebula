package main

import (
	"testing"
)

func TestProbeGood(t *testing.T) {
	t.Run("list-panes timeout fails the combined probe", func(t *testing.T) {
		tabs := ProbeCall{ExitCode: 0, Stdout: "[]"}
		if probeGoodOutput(tabs, ProbeCall{Timeout: true}) {
			t.Fatal("probeGoodOutput() = true, want false")
		}
	})
	t.Run("good first probe output is healthy", func(t *testing.T) {
		good := ProbeCall{ExitCode: 0, Stdout: "[]"}
		if !probeGoodOutput(good, good) {
			t.Fatal("probeGoodOutput() = false for valid first-probe outputs")
		}
	})
}

func TestProbeGoodOutputBothCommandOutputsMustBeJSONArrays(t *testing.T) {
	good := ProbeCall{ExitCode: 0, Stdout: "[]"}
	if !probeGoodOutput(good, good) {
		t.Fatal("probeGoodOutput() = false for JSON arrays")
	}
	if probeGoodOutput(good, ProbeCall{Timeout: true}) {
		t.Fatal("probeGoodOutput() = true when second command times out")
	}
}
