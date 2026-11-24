package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	reportTimeout = 30 * time.Second
	// reportCap bounds the workspaces named in one DM. The full list goes to a file.
	reportCap = 10
)

// reportState is the set of kept workspace roots already reported. Only new roots reach the DM.
type reportState struct {
	Reported []string `json:"reported"`
}

// stateFile is <state>/dev-hygiene/reported.json. The state directory is XDG_STATE_HOME or ~/.local/state.
func stateFile(stateDir string) string {
	return filepath.Join(stateDir, "dev-hygiene", "reported.json")
}

// loadReportState reads the reported set. A missing file means nothing has been reported yet.
func loadReportState(path string) (reportState, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // Runtime state under the operator's XDG state dir.
	if errors.Is(err, fs.ErrNotExist) {
		return reportState{}, nil
	}
	if err != nil {
		return reportState{}, fmt.Errorf("read report state %s: %w", path, err)
	}
	var st reportState
	if err := json.Unmarshal(raw, &st); err != nil {
		return reportState{}, fmt.Errorf("parse report state %s: %w", path, err)
	}
	return st, nil
}

// saveReportState writes the reported set through a temp file and rename.
func saveReportState(path string, st reportState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create report state dir: %w", err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode report state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write report state %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace report state %s: %w", path, err)
	}
	return nil
}

// newKept returns the kept workspaces not in the reported set. A workspace is keyed by its root and unlanded IDs,
// so a change to its unlanded work is reported again.
func newKept(kepts []kept, st reportState) []kept {
	var fresh []kept
	for _, k := range kepts {
		if !slices.Contains(st.Reported, reportKey(k)) {
			fresh = append(fresh, k)
		}
	}
	return fresh
}

// reportKey is the reported-set entry for one kept workspace.
func reportKey(k kept) string {
	return k.key
}

// reportKeyOf is the reported-set key: the root and its unlanded change IDs, tab separated.
// Notes such as the untracked-file count stay out, so routine build churn does not re-DM a workspace.
func reportKeyOf(root string, unlanded []string) string {
	return root + "\t" + strings.Join(unlanded, "|")
}

// keyRoot returns the workspace root a reported-set key names.
func keyRoot(key string) string {
	root, _, _ := strings.Cut(key, "\t")
	return root
}

// pruneReported keeps entries whose workspace root still exists. A recent or live workspace is not kept this run,
// but its root still exists, so its key stays and the same unlanded work is not re-DMed.
func pruneReported(st reportState) reportState {
	var live []string
	for _, key := range st.Reported {
		if info, err := os.Stat(keyRoot(key)); err == nil && info.IsDir() {
			live = append(live, key)
		}
	}
	return reportState{Reported: live}
}

// reportKept sends one Cotal DM for workspaces not reported before, capped, and writes the full list to fullPath.
// A dry run prints the DM and writes nothing. The reported set is saved only after a successful send.
func reportKept(ctx context.Context, cfg config, kepts []kept, st reportState, fullPath string, out *printer) (reportState, error) {
	fresh := newKept(kepts, st)
	if len(fresh) == 0 {
		return st, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return st, fmt.Errorf("resolve hostname: %w", err)
	}
	var lines []string
	for _, k := range fresh {
		lines = append(lines, k.line)
	}
	text := keptText(host, fresh, fullPath)
	if !cfg.apply {
		out.printf("would write %s and send to %s: %s\n", fullPath, cfg.to, text)
		return st, nil
	}
	if err := writeFullList(fullPath, lines); err != nil {
		return st, err
	}
	if err := sendDM(ctx, cfg, text); err != nil {
		return st, err
	}
	for _, k := range fresh {
		// One key per root: a changed unlanded set replaces the old key rather than piling up beside it.
		st.Reported = slices.DeleteFunc(st.Reported, func(key string) bool { return keyRoot(key) == k.root })
		st.Reported = append(st.Reported, reportKey(k))
	}
	return st, nil
}

// sendDM sends text to cfg.to over Cotal, bounded by reportTimeout.
func sendDM(ctx context.Context, cfg config, text string) error {
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	args := []string{"send", "--to", cfg.to, text, "--space", "main"}
	if cfg.server != "" {
		args = append(args, "--server", cfg.server)
	}
	if cfg.creds != "" {
		// Cotal checks the folders of the path it is given, so pass the symlink as is, not its target.
		// Stat only so a creds file sops has not placed yet fails here, not inside cotal.
		if _, err := os.Stat(cfg.creds); err != nil {
			return fmt.Errorf("stat -creds: %w", err)
		}
		args = append(args, "--creds", cfg.creds)
	}
	if _, err := commandRun(ctx, "", nil, cfg.cotalBin, args...); err != nil {
		return fmt.Errorf("send report to %s: %w", cfg.to, err)
	}
	return nil
}

// failureText is the DM body for a failed apply run, so a persistent error is noticed before the disk fills.
func failureText(host string, err error) string {
	text := fmt.Sprintf("dev-hygiene on %s failed: %v", host, err)
	const limit = 1500
	if len(text) > limit {
		text = text[:limit] + " …"
	}
	return text
}

// keptText builds the DM body: a header, then one line per kept workspace up to reportCap, then a count of the rest.
func keptText(host string, fresh []kept, fullPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "dev-hygiene on %s kept %d idle jj workspace(s) it did not delete (unlanded, dirty, or not inspected). Full list: %s", host, len(fresh), fullPath)
	shown := min(len(fresh), reportCap)
	for _, k := range fresh[:shown] {
		fmt.Fprintf(&b, "\n- %s", k.line)
	}
	if more := len(fresh) - shown; more > 0 {
		fmt.Fprintf(&b, "\n+%d more", more)
	}
	return b.String()
}

// writeFullList writes every new kept line to path, so the DM can name one file instead of listing all.
func writeFullList(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write full report %s: %w", path, err)
	}
	return nil
}
