// Package echo parses the echo lines Alloy writes to its log.
package echo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"time"
)

// Result holds each echoed line per stream and its Alloy log time (seconds).
// The limiter refills on that clock.
type Result struct {
	Lines   map[string][]string
	At      map[string][]float64
	Seconds float64
}

// Count returns how many lines were echoed across all streams.
func (r Result) Count() int {
	n := 0
	for _, ls := range r.Lines {
		n += len(ls)
	}
	return n
}

var (
	echoLineRe = regexp.MustCompile(`(receiver|component_id)=loki\.echo\.e .*entry=`)
	streamIDRe = regexp.MustCompile(`stream_id=\\"([^\\"]+)\\"`)
	entryRe    = regexp.MustCompile(` entry="((?:[^"\\]|\\.)*)"`)
	tsRe       = regexp.MustCompile(`^ts=(\S+)`)
)

// ReadEchoed parses the echo lines Alloy wrote so far.
func ReadEchoed(path string) (Result, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the Alloy log this run created in its temp directory.
	if err != nil {
		return Result{}, fmt.Errorf("read alloy log: %w", err)
	}
	return Parse(raw)
}

// Parse reads the echo lines in log, grouped by stream_id. A malformed entry is an error.
func Parse(log []byte) (Result, error) {
	out := Result{Lines: map[string][]string{}, At: map[string][]float64{}}
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Text()
		// 1.16 tags echo lines `receiver=`, 1.18 `component_id=`.
		if !echoLineRe.MatchString(line) {
			continue
		}
		id := "?"
		if m := streamIDRe.FindStringSubmatch(line); m != nil {
			id = m[1]
		}
		entry := ""
		if m := entryRe.FindStringSubmatch(line); m != nil {
			entry = m[1]
		}
		var decoded string
		if err := json.Unmarshal([]byte(`"`+entry+`"`), &decoded); err != nil {
			return Result{}, fmt.Errorf("decode echo entry: %w", err)
		}
		// Unparseable timestamps become NaN, as Date.parse returned NaN.
		ts := math.NaN()
		if m := tsRe.FindStringSubmatch(line); m != nil {
			if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				ts = float64(t.UnixNano()) / 1e9
			}
		}
		out.Lines[id] = append(out.Lines[id], decoded)
		out.At[id] = append(out.At[id], ts)
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("scan alloy log: %w", err)
	}
	return out, nil
}
