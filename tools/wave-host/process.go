package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type Identity struct {
	PID        int    `json:"pid"`
	StartToken string `json:"startToken"`
}

func identityReadError(pid int, err error) error {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return fmt.Errorf("process %d: %w", pid, os.ErrNotExist)
	}
	return err
}

func parseLinuxStat(data string) (string, error) {
	_, rest, found := strings.CutLast(data, ")")
	if !found {
		return "", errors.New("invalid proc stat")
	}
	fields := strings.Fields(rest)
	if len(fields) <= 19 {
		return "", errors.New("short proc stat")
	}
	token := fields[19]
	if _, err := strconv.ParseUint(token, 10, 64); err != nil {
		return "", fmt.Errorf("invalid proc start time: %w", err)
	}
	return token, nil
}

func darwinIdentity(pid int, sec int64, usec int64) (Identity, error) {
	if pid <= 0 || sec <= 0 || usec < 0 || usec >= 1000000 {
		return Identity{}, errors.New("invalid process start time")
	}
	return Identity{PID: pid, StartToken: strconv.FormatInt(sec*1000000+usec, 10)}, nil
}

// normalizeStartToken maps a legacy six-digit sec.usec record to integer microseconds;
// any other token, malformed dotted ones included, is returned unchanged.
func normalizeStartToken(recorded string) string {
	sec, usec, legacy := strings.Cut(recorded, ".")
	if !legacy || len(usec) != 6 {
		return recorded
	}
	s, errS := strconv.ParseUint(sec, 10, 64)
	u, errU := strconv.ParseUint(usec, 10, 64)
	if errS != nil || errU != nil {
		return recorded
	}
	return strconv.FormatUint(s*1000000+u, 10)
}

// sameStartToken reports whether recorded names current; reject malformed records first.
func sameStartToken(recorded, current string) bool {
	return normalizeStartToken(recorded) == current
}

// isMalformedDottedToken reports a dotted token that is not sec.usec; callers treat it as unknown, never gone.
func isMalformedDottedToken(recorded string) bool {
	sec, usec, legacy := strings.Cut(recorded, ".")
	if !legacy {
		return false
	}
	if len(usec) != 6 {
		return true
	}
	_, errS := strconv.ParseUint(sec, 10, 64)
	_, errU := strconv.ParseUint(usec, 10, 64)
	return errS != nil || errU != nil
}

func identifyDarwin(pid int, read func(int) (int, int64, int64, error)) (Identity, error) {
	if pid <= 0 {
		return Identity{}, errors.New("invalid pid")
	}
	actual, sec, usec, err := read(pid)
	if err != nil {
		return Identity{}, fmt.Errorf("read process identity: %w", identityReadError(pid, err))
	}
	if actual != pid {
		return Identity{}, errors.New("process identity mismatch")
	}
	return darwinIdentity(pid, sec, usec)
}

// runProcessIdentity prints one live process's start token. Exit 0 printed, 3 absent,
// 1 unreadable, 2 usage; no token is printed on refusal.
func runProcessIdentity(args []string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("process-identity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pid := flags.Int("pid", 0, "process id")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *pid <= 0 {
		if _, err := fmt.Fprintln(stderr, "usage: wave-host process-identity --pid N"); err != nil {
			return 1
		}
		return 2
	}
	id, err := Identify(*pid)
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		if errors.Is(err, os.ErrNotExist) {
			return 3
		}
		return 1
	}
	if _, err := fmt.Fprintln(out, id.StartToken); err != nil {
		return 1
	}
	return 0
}
