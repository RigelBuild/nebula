// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"strings"
	"testing"
)

func TestSeatListV2PreservesOperationOrder(t *testing.T) {
	fs := newMemoryStoreFS()
	const path = "/state/wave-seats.json"
	current := v2TestList(0)
	for _, id := range []string{"z", "a"} {
		op := Operation{OperationID: id, Principal: Principal{Kind: "operator", Operator: "test"}, Request: new(id), Name: new("alpha")}
		var err error
		current, err = CommitLocked(path, current, []SeatEntryV2{v2TestSeat("alpha")}, op, fs)
		if err != nil {
			t.Fatal(err)
		}
	}
	want := string(fs.files[path])
	if strings.Index(want, `test\",\"z`) >= strings.Index(want, `test\",\"a`) {
		t.Fatal("operation keys are not in insertion order")
	}
	parsed, err := ParseSeatListV2(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SerializeSeatListV2(parsed)
	if err != nil || string(got) != want {
		t.Fatalf("round trip changed bytes: %v\ngot %s\nwant %s", err, got, want)
	}
}

func TestSeatListV2PreservesModelOrder(t *testing.T) {
	text := strings.Replace(v2TestListWithRaw(`"x":1`), `,"x":1`, "", 1)
	text = strings.Replace(text, `"limits":{}`, `"limits":{"perModel":{"z":{"tokens":1},"a":{"rpm":2}}}`, 1)
	list, err := ParseSeatListV2(text)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SerializeSeatListV2(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(got), `"z"`) >= strings.Index(string(got), `"a"`) {
		t.Fatalf("model order changed: %s", got)
	}
}

func TestSerializeSeatListV2SortsNewModelKeys(t *testing.T) {
	one := uint64(1)
	models := map[string]ModelLimit{}
	for _, name := range []string{"m", "b", "y", "d", "k", "a"} {
		models[name] = ModelLimit{Tokens: &one}
	}
	list := v2TestList(1, "alpha")
	list.Seats[0].Key.Limits.PerModel = models
	first, err := SerializeSeatListV2(list)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := SerializeSeatListV2(list)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("serialization is not deterministic:\n%s\n%s", first, again)
		}
	}
	text := string(first)
	for _, pair := range [][2]string{{`"a"`, `"b"`}, {`"d"`, `"k"`}, {`"m"`, `"y"`}} {
		if strings.Index(text, pair[0]) >= strings.Index(text, pair[1]) {
			t.Fatalf("new model keys are not sorted: %s", text)
		}
	}
}
