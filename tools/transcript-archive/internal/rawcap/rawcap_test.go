package rawcap

import (
	"errors"
	"testing"
)

const capBytes = uint64(500) << 30

func TestCheckBoundary(t *testing.T) {
	cases := []struct {
		name     string
		json     string
		wantErr  bool
		wantOver bool
	}{
		{name: "under cap passes", json: `{"total_size": 536870911999}`},
		{name: "at cap refuses", json: `{"total_size": 536870912000}`, wantErr: true, wantOver: true},
		{name: "over cap refuses", json: `{"total_size": 536870912001}`, wantErr: true, wantOver: true},
		{name: "missing total_size fails closed", json: `{"total_bytes": 1}`, wantErr: true},
		{name: "malformed json fails closed", json: `{`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check([]byte(tc.json), capBytes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Check() error = %v, want error %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrOverCap); got != tc.wantOver {
				t.Fatalf("errors.Is(ErrOverCap) = %v, want %v (err %v)", got, tc.wantOver, err)
			}
		})
	}
}
