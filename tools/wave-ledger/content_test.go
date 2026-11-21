package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestReadContent(t *testing.T) {
	previousLimit := maxContentLineSize
	maxContentLineSize = 1024
	defer func() { maxContentLineSize = previousLimit }()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	frame := func(s string) []byte { return enc.EncodeAll([]byte(s), nil) }
	first := frame("{\"v\":1,\"session_id\":\"first\"}\n")
	second := frame("{\"v\":1,\"session_id\":\"second\"}\n")
	for _, tc := range []struct {
		name string
		data []byte
		want int
		bad  int
	}{
		{name: "concatenated_frames", data: append(append([]byte{}, first...), second...), want: 2},
		{name: "bad_frame", data: append(append([]byte{}, first...), second[:len(second)-2]...), want: 1, bad: 1},
		{name: "invalid_row", data: append(append([]byte{}, first...), frame("{invalid}\n")...), want: 1, bad: 1},
		{name: "unterminated_row", data: append(append([]byte{}, first...), frame("{invalid")...), want: 1, bad: 1},
		{name: "oversized_row", data: append(append([]byte{}, first...), frame(strings.Repeat("x", maxContentLineSize+1)+"\n")...), want: 1, bad: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "content"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "content", "2026-11-09.jsonl.zst"), tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
			var got []string
			bad, err := ReadContent(dir, day, day, func(row ContentRow) error { got = append(got, row.SessionID); return nil })
			if err != nil || bad != tc.bad || len(got) != tc.want {
				t.Fatalf("got %v bad %d err %v", got, bad, err)
			}
			if len(got) > 0 && got[0] != "first" {
				t.Fatalf("wrong order: %v", got)
			}
		})
	}
}

func TestReadContentMidLineTruncation(t *testing.T) {
	var compressed bytes.Buffer
	enc, err := zstd.NewWriter(&compressed, zstd.WithEncoderCRC(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write([]byte("{\"v\":1,\"session_id\":\"first\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write([]byte("{\"v\":1,\"session_id\":\"" + strings.Repeat("x", 300000))); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	data := compressed.Bytes()
	var got []string
	bad, err := readContentFile(bytes.NewReader(data[:len(data)-1]), func(row ContentRow) error { got = append(got, row.SessionID); return nil })
	if err != nil || !bad || len(got) != 1 || got[0] != "first" {
		t.Fatalf("got %v bad %v error %v", got, bad, err)
	}
}

func TestReadContentTornTailOnRowBoundary(t *testing.T) {
	var compressed bytes.Buffer
	enc, err := zstd.NewWriter(&compressed, zstd.WithEncoderCRC(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write([]byte("{\"v\":1,\"session_id\":\"complete\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	data := compressed.Bytes()
	if len(data) < 2 {
		t.Fatal("compressed content too short to truncate")
	}
	var got []string
	bad, err := readContentFile(bytes.NewReader(data[:len(data)-1]), func(row ContentRow) error {
		got = append(got, row.SessionID)
		return nil
	})
	if err != nil || !bad || len(got) != 1 || got[0] != "complete" {
		t.Fatalf("got %v bad %v error %v", got, bad, err)
	}
}

func TestReadContentBadPart(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	data := enc.EncodeAll([]byte("{\"v\":1,\"session_id\":\"s\",\"parts\":[{\"type\":\"text\",\"text\":\"private\"},42]}\n"), nil)
	agent := "agent"
	d := newDriverTotals([]Row{{Kind: kindMessage, SessionID: "s", Agent: &agent}})
	bad, err := readContentFile(bytes.NewReader(data), d.addContent)
	if err != nil || !bad || len(d.bytes) != 0 {
		t.Fatalf("bad = %v, error = %v, bytes = %v", bad, err, d.bytes)
	}
}
