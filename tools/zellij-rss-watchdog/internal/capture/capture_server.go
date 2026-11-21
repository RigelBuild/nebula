package capture

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/proc"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

// errIdentityChanged fails a capture step when the server identity moved mid-capture.
var errIdentityChanged = errors.New("server identity changed")

// CaptureServer writes one capture directory for the server identity and prunes old captures.
func CaptureServer(ctx context.Context, io seam.IO, identity string, rssBytes int64) error {
	directory := filepath.Join(io.Config().CaptureRoot, CaptureName(io.Now()))
	if err := io.Mkdir(directory); err != nil {
		return err
	}
	var errs []string
	pid, err := proc.IdentityPid(identity)
	if err != nil {
		return err
	}
	verify := func() error {
		if io.Identity() != identity {
			return errIdentityChanged
		}
		return nil
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"zellij-logs", func() error { return captureZellijLogs(ctx, io, directory, &errs) }},
		{"smaps", func() error { return captureProcFile(io, verify, pid, "smaps", directory, "smaps.txt") }},
		{"status", func() error { return captureProcFile(io, verify, pid, "status", directory, "status.txt") }},
		{"socket-peers", func() error { return captureSocketPeers(ctx, io, directory, pid, verify) }},
		{"mapping-dumps", func() error {
			if err := verify(); err != nil {
				return err
			}
			return captureMaps(ctx, io, pid, directory, identity, &errs)
		}},
		{"unit-journal", func() error { return captureUnitJournal(ctx, io, directory) }},
		{"zellij-clients", func() error { return captureZellijClients(ctx, io, directory) }},
	}
	for _, step := range steps {
		captureStep(step.name, &errs, step.run)
	}
	if err := io.WriteText(filepath.Join(directory, "sample-rss.txt"), fmt.Sprintf("%d\n", rssBytes)); err != nil {
		return err
	}
	if len(errs) > 0 {
		if err := io.WriteText(filepath.Join(directory, "errors.txt"), strings.Join(errs, "\n")+"\n"); err != nil {
			return err
		}
		io.Log("error", fmt.Sprintf("event=capture_step_failure path=%s steps=%d", Logfmt(directory), len(errs)))
	}
	if err := pruneCaptureRoot(io); err != nil {
		return err
	}
	io.Log("info", fmt.Sprintf("event=capture pid=%s path=%s rss_bytes=%d", pid, Logfmt(directory), rssBytes))
	return nil
}

// captureProcFile copies one /proc/<pid> text file into the capture, after an identity check.
func captureProcFile(io seam.IO, verify func() error, pid, name, directory, out string) error {
	if err := verify(); err != nil {
		return err
	}
	text, err := io.ReadText(io.Config().ProcRoot + "/" + pid + "/" + name)
	if err != nil {
		return err
	}
	return io.WriteText(filepath.Join(directory, out), text)
}

// captureUnitJournal writes the last ten minutes of the configured user unit's journal.
func captureUnitJournal(ctx context.Context, io seam.IO, directory string) error {
	result, err := RunAsUser(ctx, io, []string{
		EnvCommand,
		"XDG_RUNTIME_DIR=" + io.Config().RuntimeDir,
		"journalctl",
		"--user",
		"-u",
		io.Config().JournalUnit,
		"--since",
		"-10min",
		"--no-pager",
	})
	if err != nil {
		return fmt.Errorf("journal for unit %s: %w", io.Config().JournalUnit, err)
	}
	return io.WriteText(filepath.Join(directory, "unit-journal.txt"), result.Stdout)
}

// captureZellijClients writes the zellij session's client list.
func captureZellijClients(ctx context.Context, io seam.IO, directory string) error {
	session := io.Config().Session
	result, err := RunAsUser(ctx, io, []string{
		EnvCommand,
		"TMPDIR=" + io.Config().ClientTmpDir,
		"ZELLIJ_SESSION_NAME=" + session,
		"XDG_RUNTIME_DIR=" + io.Config().RuntimeDir,
		"zellij",
		"--session",
		session,
		"action",
		"list-clients",
	})
	if err != nil {
		return err
	}
	return io.WriteText(filepath.Join(directory, "zellij-clients.txt"), result.Stdout)
}

// captureZellijLogs copies the bounded log tails; a per-file write failure is recorded, not fatal.
func captureZellijLogs(ctx context.Context, io seam.IO, directory string, errs *[]string) error {
	logs, err := io.ReadUserLogs(ctx, MaxLogBytes)
	if err != nil {
		return err
	}
	for _, message := range logs.Errors {
		*errs = append(*errs, "zellij-logs: "+message)
	}
	for _, file := range logs.Files {
		if err := io.WriteBytes(filepath.Join(directory, file.Name), file.Contents); err != nil {
			*errs = append(*errs, fmt.Sprintf("zellij-logs %s: %s", file.Name, err))
		}
	}
	return nil
}

// captureSocketPeers records the server's ss rows and each peer's exe and cmdline.
func captureSocketPeers(ctx context.Context, io seam.IO, directory, pid string, verify func() error) error {
	if err := verify(); err != nil {
		return err
	}
	sockets, err := io.Run(ctx, "ss", []string{"-xp"}, command.CommandTimeout)
	if err != nil {
		return err
	}
	serverLines, peerPIDs := parseSsPeers(sockets.Stdout, pid)
	if err := io.WriteText(filepath.Join(directory, "ss.txt"), strings.Join(serverLines, "\n")); err != nil {
		return err
	}
	if len(serverLines) == 0 {
		if err := io.WriteText(filepath.Join(directory, "peers.txt"), "\n"); err != nil {
			return err
		}
		return errors.New("ss output has no server row")
	}
	peers := make([]string, 0, len(peerPIDs))
	for _, peer := range peerPIDs {
		peers = append(peers, describePeer(ctx, io, peer, verify))
	}
	return io.WriteText(filepath.Join(directory, "peers.txt"), strings.Join(peers, "\n")+"\n")
}

// describePeer returns one peers.txt line; a failure becomes an error line, never a skipped peer.
func describePeer(ctx context.Context, io seam.IO, peer string, verify func() error) string {
	if err := verify(); err != nil {
		return fmt.Sprintf("pid=%s error=%s", peer, Logfmt(err.Error()))
	}
	exeResult, err := io.Run(ctx, "readlink", []string{"-f", io.Config().ProcRoot + "/" + peer + "/exe"}, command.CommandTimeout)
	if err != nil {
		return fmt.Sprintf("pid=%s error=%s", peer, Logfmt(err.Error()))
	}
	exe := strings.TrimSpace(exeResult.Stdout)
	if err := verify(); err != nil {
		return fmt.Sprintf("pid=%s error=%s", peer, Logfmt(err.Error()))
	}
	cmdline, err := io.ReadText(io.Config().ProcRoot + "/" + peer + "/cmdline")
	if err != nil {
		return fmt.Sprintf("pid=%s error=%s", peer, Logfmt(err.Error()))
	}
	return fmt.Sprintf("pid=%s exe=%s cmdline=%s", peer, Logfmt(exe), Logfmt(joinNonEmpty(cmdline)))
}

// joinNonEmpty joins NUL-separated argv entries with spaces, dropping empty entries.
func joinNonEmpty(cmdline string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(cmdline, "\x00") {
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(part)
	}
	return b.String()
}

// pruneCaptureRoot removes capture directories beyond the newest MaxCaptures.
func pruneCaptureRoot(io seam.IO) error {
	names, err := io.ReadDir(io.Config().CaptureRoot)
	if err != nil {
		return err
	}
	var dirs []string
	for _, name := range names {
		entry, err := io.Stat(filepath.Join(io.Config().CaptureRoot, name))
		if err != nil {
			return err
		}
		if entry.IsDirectory {
			dirs = append(dirs, filepath.Join(io.Config().CaptureRoot, entry.Name))
		}
	}
	for _, path := range PruneCaptures(dirs) {
		if err := io.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
