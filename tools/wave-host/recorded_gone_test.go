package main

import (
	"errors"
	"os"
	"testing"
)

func TestRecordedGone(t *testing.T) {
	record := seatRecord{HolderPID: 1, HolderToken: "old", AgentPID: 2, AgentToken: "old"}
	cases := []struct {
		name     string
		identify func(int) (Identity, error)
		wantErr  bool
	}{
		{name: "both gone", identify: func(int) (Identity, error) { return Identity{}, os.ErrNotExist }},
		{name: "both reused", identify: func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "new"}, nil }},
		{name: "holder alive", identify: func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "old"}, nil }, wantErr: true},
		{name: "agent alive", identify: func(pid int) (Identity, error) {
			if pid == 1 {
				return Identity{}, os.ErrNotExist
			}
			return Identity{PID: pid, StartToken: "old"}, nil
		}, wantErr: true},
		{name: "unknown", identify: func(int) (Identity, error) { return Identity{}, errors.New("denied") }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := seatHost{identify: tc.identify}
			if err := h.recordedGone(record); (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
