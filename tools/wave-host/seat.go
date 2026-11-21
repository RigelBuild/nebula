package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec" //nolint:depguard // The adapter runs launchctl and the caller's tmux executable.
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const seatTimeout = 10 * time.Second

const seatRunning = "running"

type seatRecord struct {
	State       string `json:"state"`
	Label       string `json:"label"`
	Socket      string `json:"socket"`
	HolderPID   int    `json:"holderPid,omitempty"`
	HolderToken string `json:"holderToken,omitempty"`
	AgentPID    int    `json:"agentPid,omitempty"`
	AgentToken  string `json:"agentToken,omitempty"`
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type seatHost struct {
	run      commandRunner
	identify func(int) (Identity, error)
	connect  func(context.Context, string) error
	children func(int) ([]int, error)
	dir      string
	domain   string
	timeout  time.Duration
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	//nolint:gosec // Only launchctl and the caller-resolved tmux are executed.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func connectSeat(ctx context.Context, path string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	return conn.Close()
}

var launchPID = regexp.MustCompile(`(?m)^\s*pid\s*=\s*([0-9]+)\s*$`)

func (h seatHost) print(ctx context.Context, label string) (int, bool, error) {
	output, err := h.run(ctx, "launchctl", "print", h.domain+"/"+label)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 113 {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("launchctl print: %w: %s", err, strings.TrimSpace(string(output)))
	}
	match := launchPID.FindSubmatch(output)
	if len(match) != 2 {
		return 0, true, nil
	}
	pid, err := strconv.Atoi(string(match[1]))
	if err != nil || pid <= 0 {
		return 0, false, errors.New("launchctl print has invalid pid")
	}
	return pid, true, nil
}

func (h seatHost) removeLabel(ctx context.Context, label string) error {
	_, present, err := h.print(ctx, label)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	out, err := h.run(ctx, "launchctl", "bootout", h.domain+"/"+label)
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for {
		_, present, err = h.print(ctx, label)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for launchd removal: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func loadSeat(path string) (seatRecord, error) {
	//nolint:gosec // The path names a seat record under the operator's runtime directory.
	data, err := os.ReadFile(path)
	if err != nil {
		return seatRecord{}, err
	}
	var record seatRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return seatRecord{}, fmt.Errorf("decode seat record: %w", err)
	}
	if (record.State != "pending" && record.State != seatRunning) || record.Label == "" || record.Socket == "" {
		return seatRecord{}, errors.New("invalid seat record")
	}
	return record, nil
}

func saveSeat(path string, record seatRecord) (err error) {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".seat-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() {
		//nolint:gosec // temp is created in the trusted seat state directory above.
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	//nolint:gosec // Both paths are in the trusted seat state directory.
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	//nolint:gosec // The path names a seat state directory under the operator's home.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return errors.Join(err, dir.Close())
	}
	return dir.Close()
}

func clearSeat(path string) error {
	//nolint:gosec // The path names a seat record under the operator's runtime directory.
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	//nolint:gosec // The directory belongs to the trusted runtime seat state path.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return errors.Join(err, dir.Close())
	}
	return dir.Close()
}

func seatPlist(label, tmux, socket, conf, dir, path string) (string, error) {
	args := []string{tmux, "-S", socket, "-f", conf, "-D"}
	var b strings.Builder
	writeValue := func(value string) error {
		return xml.EscapeText(&b, []byte(value))
	}
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?><!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\"><plist version=\"1.0\"><dict><key>Label</key><string>")
	if err := writeValue(label); err != nil {
		return "", err
	}
	b.WriteString("</string><key>ProgramArguments</key><array>")
	for _, arg := range args {
		b.WriteString("<string>")
		if err := writeValue(arg); err != nil {
			return "", err
		}
		b.WriteString("</string>")
	}
	b.WriteString("</array><key>RunAtLoad</key><true/><key>EnvironmentVariables</key><dict><key>TMPDIR</key><string>/tmp/</string><key>PATH</key><string>")
	if err := writeValue(path); err != nil {
		return "", err
	}
	b.WriteString("</string></dict>")
	for _, entry := range []struct{ key, path string }{{"StandardOutPath", filepath.Join(dir, label+".out.log")}, {"StandardErrorPath", filepath.Join(dir, label+".err.log")}} {
		b.WriteString("<key>" + entry.key + "</key><string>")
		if err := writeValue(entry.path); err != nil {
			return "", err
		}
		b.WriteString("</string>")
	}
	b.WriteString("</dict></plist>")
	return b.String(), nil
}

type seatStartArgs struct {
	name, socket, conf, cwd, tmux, path string
	command                             []string
}

func (h seatHost) locked(ctx context.Context, name string, fn func(string, string) error) (err error) {
	if !seatName.MatchString(name) {
		return errors.New("invalid seat name")
	}
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		return err
	}
	// Only wave-host creates a seat label, while holding this per-seat lock.
	lock, err := lockFile(ctx, filepath.Join(h.dir, name+".lock"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	return fn(filepath.Join(h.dir, name+".json"), "build.rigel.wave-seat."+name)
}

func (h seatHost) recordedGone(record seatRecord) error {
	for _, id := range []Identity{{PID: record.HolderPID, StartToken: record.HolderToken}, {PID: record.AgentPID, StartToken: record.AgentToken}} {
		if id.PID <= 0 || id.StartToken == "" {
			return errors.New("seat identity unknown")
		}
		current, err := h.identify(id.PID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || current.PID != id.PID || current.StartToken == "" {
			return fmt.Errorf("seat identity unknown for pid %d: %w", id.PID, errors.Join(errors.New("identity unavailable"), err))
		}
		if isMalformedDottedToken(id.StartToken) {
			return fmt.Errorf("seat identity unknown for pid %d: recorded start token is unreadable", id.PID)
		}
		if sameStartToken(id.StartToken, current.StartToken) {
			return fmt.Errorf("seat process %d is still alive", id.PID)
		}
	}
	return nil
}

func (h seatHost) stop(ctx context.Context, name string) error {
	return h.locked(ctx, name, func(path, label string) error {
		record, err := loadSeat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && record.Label != label {
			return errors.New("seat label mismatch")
		}
		hasRecord := err == nil
		bounded, cancel := context.WithTimeout(ctx, h.timeout)
		defer cancel()
		_, present, printErr := h.print(bounded, label)
		if printErr != nil {
			return printErr
		}
		if present && !hasRecord {
			return errors.New("orphaned seat label has no record")
		}
		var killErr error
		if hasRecord && present {
			var output []byte
			output, killErr = h.run(bounded, "tmux", "-S", record.Socket, "kill-server")
			if killErr != nil {
				killErr = fmt.Errorf("tmux kill-server: %w: %s", killErr, strings.TrimSpace(string(output)))
			}
		}
		if err := h.removeLabel(bounded, label); err != nil {
			return errors.Join(killErr, err)
		}
		if killErr != nil && hasRecord && record.State != seatRunning {
			return errors.Join(killErr, errors.New("seat identity unknown"))
		}
		if hasRecord && record.State == seatRunning {
			if err := h.recordedGone(record); err != nil {
				return errors.Join(killErr, err)
			}
		}
		return clearSeat(path)
	})
}

func (h seatHost) start(ctx context.Context, a seatStartArgs) (holder, agent int, err error) {
	err = h.locked(ctx, a.name, func(path, label string) error {
		if len(a.socket) > 103 {
			return errors.New("seat socket path exceeds 103 bytes")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if a.socket != filepath.Join(home, "agents/run/seats", a.name+".sock") {
			return errors.New("seat socket must be under ~/agents/run/seats")
		}
		record, err := loadSeat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && record.Label != label {
			return errors.New("seat label mismatch")
		}
		if err == nil && record.State == seatRunning {
			if err := h.recordedGone(record); err != nil {
				return err
			}
		}
		bounded, cancel := context.WithTimeout(ctx, h.timeout)
		defer cancel()
		if errors.Is(err, os.ErrNotExist) {
			_, present, printErr := h.print(bounded, label)
			if printErr != nil {
				return printErr
			}
			if present {
				return errors.New("orphaned seat label has no record")
			}
		}
		if err := h.removeLabel(bounded, label); err != nil {
			return err
		}
		if err := clearSeat(path); err != nil {
			return err
		}
		return h.bootstrapSeat(ctx, bounded, a, path, label, &holder, &agent)
	})
	return holder, agent, err
}

func (h seatHost) bootstrapSeat(ctx, bounded context.Context, a seatStartArgs, path, label string, holder, agent *int) error {
	if err := os.MkdirAll(filepath.Dir(a.socket), 0700); err != nil {
		return err
	}
	//nolint:gosec // A socket directory needs owner execute permission.
	if err := os.Chmod(filepath.Dir(a.socket), 0700); err != nil {
		return err
	}
	pending := seatRecord{State: "pending", Label: label, Socket: a.socket}
	if err := saveSeat(path, pending); err != nil {
		return err
	}
	plist, err := os.CreateTemp(h.dir, ".launch-*.plist")
	if err != nil {
		return err
	}
	plistPath := plist.Name()
	plistData, err := seatPlist(label, a.tmux, a.socket, a.conf, h.dir, a.path)
	if err != nil {
		return errors.Join(err, plist.Close(), os.Remove(plistPath))
	}
	if _, err = plist.WriteString(plistData); err != nil {
		return errors.Join(err, plist.Close(), os.Remove(plistPath))
	}
	if err = plist.Close(); err != nil {
		return errors.Join(err, os.Remove(plistPath))
	}
	out, bootErr := h.run(bounded, "launchctl", "bootstrap", h.domain, plistPath)
	removeErr := os.Remove(plistPath)
	if bootErr != nil {
		bootErr = fmt.Errorf("bootstrap: %w: %s", bootErr, strings.TrimSpace(string(out)))
	}
	startErr := errors.Join(bootErr, removeErr)
	if startErr == nil {
		var id Identity
		var agentID Identity
		id, agentID, startErr = h.waitSeat(bounded, a, label)
		*agent = agentID.PID
		*holder = id.PID
		if startErr == nil {
			running := pending
			running.State = seatRunning
			running.HolderPID = *holder
			running.HolderToken = id.StartToken
			running.AgentPID = *agent
			running.AgentToken = agentID.StartToken
			startErr = saveSeat(path, running)
			if startErr != nil {
				startErr = errors.Join(startErr, saveSeat(path, pending))
			}
		}
	}
	if startErr == nil {
		return nil
	}
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), h.timeout)
	defer cleanupCancel()
	var killErr error
	if _, statErr := os.Lstat(a.socket); statErr == nil {
		_, killErr = h.run(cleanup, a.tmux, "-S", a.socket, "kill-server")
		if killErr != nil {
			killErr = fmt.Errorf("cleanup tmux kill-server: %w", killErr)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		killErr = fmt.Errorf("inspect seat socket: %w", statErr)
	}
	if cleanupErr := h.removeLabel(cleanup, label); cleanupErr != nil {
		return errors.Join(startErr, killErr, fmt.Errorf("cleanup: %w", cleanupErr))
	}
	return errors.Join(startErr, clearSeat(path))
}

func (h seatHost) waitSeat(ctx context.Context, a seatStartArgs, label string) (Identity, Identity, error) {
	var holder int
	for {
		pid, present, err := h.print(ctx, label)
		if err != nil {
			return Identity{}, Identity{}, err
		}
		if present && pid > 0 {
			holder = pid
			break
		}
		select {
		case <-ctx.Done():
			return Identity{}, Identity{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	id, err := h.identify(holder)
	if err != nil || id.PID != holder || id.StartToken == "" {
		return Identity{}, Identity{}, errors.Join(errors.New("invalid holder identity"), err)
	}
	for {
		connectErr := h.connect(ctx, a.socket)
		if connectErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return Identity{}, Identity{}, fmt.Errorf("waiting for socket: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	args := make([]string, 0, 14+len(a.command))
	args = append(args, "-N", "-S", a.socket, "new-session", "-d", "-s", "seat", "-x", "200", "-y", "50", "-c", a.cwd, "--")
	args = append(args, a.command...)
	output, err := h.run(ctx, a.tmux, args...)
	if err != nil {
		return Identity{}, Identity{}, fmt.Errorf("tmux new-session: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for {
		output, err = h.run(ctx, a.tmux, "-N", "-S", a.socket, "display", "-p", "-t", "seat", "#{pane_pid}")
		if err == nil {
			pane, parseErr := strconv.Atoi(strings.TrimSpace(string(output)))
			if parseErr == nil && pane > 0 {
				children, childErr := h.children(pane)
				if childErr == nil && len(children) > 0 && children[0] > 0 {
					agentID, identifyErr := h.identify(children[0])
					if identifyErr != nil || agentID.PID != children[0] || agentID.StartToken == "" {
						return Identity{}, Identity{}, errors.Join(errors.New("invalid agent identity"), identifyErr)
					}
					return id, agentID, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return Identity{}, Identity{}, fmt.Errorf("waiting for seat process ids: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
