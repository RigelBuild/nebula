package proc

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// IdentityUnavailable is the server identity when /proc is unreadable or ambiguous.
const IdentityUnavailable = "unavailable"

// IsDigits reports whether text is a non-empty run of ASCII digits.
func IsDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// StatStartTime returns field 22 (starttime) of a /proc/<pid>/stat line; comm may contain ")".
func StatStartTime(stat string) string {
	idx := strings.LastIndex(stat, ")")
	if idx < 0 || idx+2 > len(stat) {
		return ""
	}
	fields := strings.Split(stat[idx+2:], " ")
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// IdentityPid returns the pid half of "<pid>:<starttime>".
func IdentityPid(identity string) (string, error) {
	separator := strings.Index(identity, ":")
	if separator < 1 || !IsDigits(identity[:separator]) {
		return "", fmt.Errorf("invalid server identity: %s", identity)
	}
	return identity[:separator], nil
}

// CopyRanges copies [start, end) from mem to out in 4 MiB chunks, rejecting short reads.
func CopyRanges(mem io.ReaderAt, out io.Writer, start, end uint64) error {
	chunk := make([]byte, 4<<20)
	for address := start; address < end; {
		length := min(end-address, uint64(len(chunk)))
		if address > math.MaxInt64 || length > math.MaxInt64 {
			return fmt.Errorf("proc memory offset %#x out of range", address)
		}
		n, err := mem.ReadAt(chunk[:length], int64(address))
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n < 0 || uint64(n) != length {
			return fmt.Errorf("short proc memory read: expected=%d actual=%d", length, n)
		}
		if _, err := out.Write(chunk[:n]); err != nil {
			return err
		}
		address += length
	}
	return nil
}

// MaxSafeInteger is the largest integer a JSON number can carry exactly, 2^53-1.
const MaxSafeInteger int64 = 1<<53 - 1
