package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Resolves the operator-selected SSH executable.
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/macside"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/phase"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

const (
	defaultPhasePath = "/control/phase"
	statusPath       = "/opt/data/gateway_state.json"
	statusStaleAfter = 120 * time.Second
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "nemo-move:", err); writeErr != nil {
			return 2
		}
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected gate, probe, lock-journals, verify, inventory, mac, cutover, or restore")
	}
	switch args[0] {
	case "cutover":
		return cutover(ctx, args[1:], cutoverDeps{}, os.Stdout, os.Stderr)
	case "restore":
		return RunRestore(ctx, args[1:], os.Stdout)
	case "gate":
		return gate(ctx, args[1:])
	case "probe":
		return probe(args[1:])
	case "lock-journals":
		return lockJournals(args[1:])
	case "verify":
		return verify(ctx, args[1:])
	case "verify-stream":
		if len(args) != 2 {
			return errors.New("verify-stream requires the layout JSON")
		}
		lay, err := layout.FromJSON(args[1])
		if err != nil {
			return err
		}
		file, err := os.CreateTemp("", "nemo-move-manifest-*.json")
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, os.Stdin)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return errors.Join(err, os.Remove(file.Name()))
		}
		return errors.Join(transfer.Verify(ctx, "10000", "/", file.Name(), lay), os.Remove(file.Name()))
	case "verify-as-user":
		if len(args) != 3 {
			return errors.New("verify-as-user requires a root and the layout JSON")
		}
		lay, err := layout.FromJSON(args[2])
		if err != nil {
			return err
		}
		return transfer.VerifyAsUser(ctx, args[1], lay, os.Stdin)
	case "inventory":
		return inventory(ctx, args[1:], transfer.SSHRemote(), os.Stdout)
	case "mac":
		return macCommand(ctx, args[1:], sshRunner, transfer.SSHRemote(), os.Stdout, os.Stderr)
	case "mac-side":
		return macSide(args[1:], os.Stdout)
	case "maint-side":
		return maintSide(args[1:], os.Stdout, "/control")
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func maintSide(args []string, stdout io.Writer, controlDir string) error {
	if len(args) == 0 {
		return errors.New("maint-side requires an operation")
	}
	cutoverPath := filepath.Join(controlDir, "cutover.json")
	restorePath := filepath.Join(controlDir, "restore-prepare.json")
	phasePath := filepath.Join(controlDir, "phase")
	switch args[0] {
	case "extract":
		if len(args) != 3 {
			return errors.New("extract requires the source home and the layout JSON")
		}
		lay, err := layout.FromJSON(args[2])
		if err != nil {
			return err
		}
		manifest, err := transfer.Extract(os.Stdin, "/", transfer.Options{SourceHome: args[1], Layout: lay})
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(manifest)
	case "phase-read":
		if len(args) != 1 {
			return errors.New("phase-read takes no arguments")
		}
		value, err := phase.Read(phasePath)
		if err != nil {
			value = phase.Blocked
		}
		_, err = io.WriteString(stdout, string(value))
		return err
	case "phase-write":
		if len(args) != 2 || (args[1] != "owner" && args[1] != "blocked") {
			return errors.New("phase-write requires owner or blocked")
		}
		return phase.WriteAtomic(phasePath, phase.Value(args[1]))
	case "cutover-read":
		if len(args) != 1 {
			return errors.New("cutover-read takes no arguments")
		}
		record, err := phase.ReadCutover(cutoverPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(record)
	case "cutover-write":
		if len(args) != 3 {
			return errors.New("cutover-write requires run ID and state")
		}
		return phase.WriteCutover(cutoverPath, phase.CutoverRecord{RunID: args[1], State: args[2]})
	case "restore-read":
		if len(args) != 1 {
			return errors.New("restore-read takes no arguments")
		}
		record, err := phase.ReadRestorePrepare(restorePath)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(record)
	case "restore-write":
		if len(args) != 4 {
			return errors.New("restore-write requires backup names and nonce")
		}
		return phase.WriteRestorePrepare(restorePath, phase.RestorePrepareRecord{BackupData: args[1], BackupControl: args[2], Nonce: args[3]})
	default:
		return fmt.Errorf("unknown maint-side operation %q", args[0])
	}
}

func macSide(args []string, stdout io.Writer) (result error) {
	if len(args) != 2 {
		return errors.New("mac-side requires inventory or export and the layout JSON")
	}
	lay, err := layout.FromJSON(args[1])
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	switch args[0] {
	case "inventory":
		report, inventoryErr := macside.Inventory(home, lay, time.Now())
		return errors.Join(inventoryErr, json.NewEncoder(stdout).Encode(report))
	case "export":
		manifest, err := macside.Export(home, lay, io.Discard)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		if err := encoder.Encode(struct {
			Roots []string `json:"roots"`
		}{Roots: manifest.Roots}); err != nil {
			return fmt.Errorf("write manifest roots: %w", err)
		}
		for _, entry := range manifest.Entries {
			if err := encoder.Encode(entry); err != nil {
				return fmt.Errorf("write manifest entry: %w", err)
			}
		}
		if _, err := io.WriteString(stdout, "null\n"); err != nil {
			return fmt.Errorf("write manifest terminator: %w", err)
		}
		second, err := macside.ExportChecked(home, lay, stdout, manifest)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(manifest, second) {
			return errors.New("source changed during export")
		}
		return nil
	default:
		return fmt.Errorf("unknown mac-side command %q", args[0])
	}
}

func lockJournals(args []string) error {
	flags := flag.NewFlagSet("lock-journals", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "journal directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("lock-journals takes no positional arguments")
	}
	if *dir == "" {
		return errors.New("lock-journals requires --dir")
	}
	return transfer.LockJournals(*dir)
}

func verify(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	user := flags.String("as", "", "runtime user")
	root := flags.String("root", "", "extracted transfer root")
	manifest := flags.String("manifest", "", "source manifest")
	inventory := flags.String("inventory", "", "durable-state layout CUE file (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("verify takes no positional arguments")
	}
	if *inventory == "" {
		return errors.New("verify requires --inventory")
	}
	lay, err := layout.Load(*inventory)
	if err != nil {
		return err
	}
	return transfer.Verify(ctx, *user, *root, *manifest, lay)
}

func inventory(ctx context.Context, args []string, remote transfer.Remote, stdout io.Writer) error {
	flags := flag.NewFlagSet("inventory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	target := flags.String("mac", "", "SSH target user@host")
	sshPath := flags.String("ssh", "", "absolute SSH executable")
	macBinary := flags.String("mac-binary", "", "macOS nemo-move binary")
	source := sourceFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("inventory takes no positional arguments")
	}
	if *target == "" || *macBinary == "" {
		return errors.New("inventory requires --mac and --mac-binary")
	}
	opts, err := source.check("inventory", *target)
	if err != nil {
		return err
	}
	resolved, err := resolveSSHPath(*sshPath, exec.LookPath)
	if err != nil {
		return err
	}
	if err := transfer.CheckSourceHome(ctx, opts, *target, resolved, remote); err != nil {
		return err
	}
	rows, err := transfer.Inventory(ctx, *target, resolved, *macBinary, remote, opts.Layout)
	writeErr := json.NewEncoder(stdout).Encode(struct {
		Rows []transfer.InventoryRow `json:"rows"`
	}{Rows: rows})
	return errors.Join(err, writeErr)
}

func macCommand(ctx context.Context, args []string, stopRunner commandRunner, remote transfer.Remote, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("mac requires stop or export")
	}
	switch args[0] {
	case "stop":
		return mac(ctx, args, stopRunner)
	case "export":
		flags := flag.NewFlagSet("mac export", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		target := flags.String("mac", "", "SSH target user@host")
		sshPath := flags.String("ssh", "", "absolute SSH executable")
		macBinary := flags.String("mac-binary", "", "macOS nemo-move binary")
		spoolDir := flags.String("spool-dir", os.TempDir(), "directory for verified archive spool")
		source := sourceFlags(flags)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("mac export takes no positional arguments")
		}
		if *target == "" || *macBinary == "" {
			return errors.New("mac export requires --mac and --mac-binary")
		}
		opts, err := source.check("mac export", *target)
		if err != nil {
			return err
		}
		resolved, err := resolveSSHPath(*sshPath, exec.LookPath)
		if err != nil {
			return err
		}
		return transfer.ExportToSpool(ctx, opts, *target, resolved, *macBinary, *spoolDir, remote, stdout, stderr)
	default:
		return fmt.Errorf("unknown mac step %q", args[0])
	}
}

// sourceOptions holds the required source account flags and the layout file.
type sourceOptions struct {
	homeRoot  *string
	sshUser   *string
	inventory *string
}

func sourceFlags(flags *flag.FlagSet) sourceOptions {
	return sourceOptions{
		homeRoot:  flags.String("home-root", "", "absolute source home directory (required)"),
		sshUser:   flags.String("ssh-user", "", "SSH user that owns the source home (required)"),
		inventory: flags.String("inventory", "", "durable-state layout CUE file (required)"),
	}
}

// options loads the layout named by --inventory and returns the transfer options.
func (s sourceOptions) options() (transfer.Options, error) {
	if *s.inventory == "" {
		return transfer.Options{}, errors.New("--inventory is required")
	}
	lay, err := layout.Load(*s.inventory)
	if err != nil {
		return transfer.Options{}, err
	}
	return transfer.Options{SourceHome: *s.homeRoot, MacUser: *s.sshUser, Layout: lay}, nil
}

// check requires the source flags and a --mac target whose user is --ssh-user,
// then returns the transfer options with the layout loaded.
func (s sourceOptions) check(command, target string) (transfer.Options, error) {
	if *s.homeRoot == "" || *s.sshUser == "" {
		return transfer.Options{}, fmt.Errorf("%s requires --home-root and --ssh-user", command)
	}
	opts, err := s.options()
	if err != nil {
		return transfer.Options{}, err
	}
	if err := opts.Validate(); err != nil {
		return transfer.Options{}, err
	}
	user, _, err := transfer.SplitSSHTarget(target)
	if err != nil {
		return transfer.Options{}, err
	}
	if user != *s.sshUser {
		return transfer.Options{}, fmt.Errorf("%s requires --mac user %q to match --ssh-user %q", command, user, *s.sshUser)
	}
	return opts, nil
}

func gate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("gate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	phasePath := flags.String("phase", defaultPhasePath, "phase file")
	wait := flags.Bool("wait", false, "wait for owner phase")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("gate takes no positional arguments")
	}
	if !*wait {
		return phase.RequireOwner(*phasePath)
	}
	return waitForOwner(ctx, *phasePath, 10*time.Second)
}

func waitForOwner(ctx context.Context, phasePath string, interval time.Duration) error {
	return waitForOwnerObserved(ctx, phasePath, interval, nil)
}

func waitForOwnerObserved(ctx context.Context, phasePath string, interval time.Duration, onBlocked func()) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if phase.RequireOwner(phasePath) == nil {
			return nil
		}
		if onBlocked != nil {
			onBlocked()
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func probe(args []string) error {
	if len(args) == 0 {
		return errors.New("probe requires startup, live, or ready")
	}
	kind := args[0]
	if kind != "startup" && kind != "live" && kind != "ready" {
		return fmt.Errorf("unknown probe %q", kind)
	}
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	phasePath := flags.String("phase", defaultPhasePath, "phase file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("probe takes no positional arguments")
	}
	return probeStatus(kind, *phasePath, statusPath, time.Now())
}

func probeStatus(kind, phasePath, runtimeStatusPath string, now time.Time) error {
	value, readErr := phase.Read(phasePath)
	if readErr != nil || value != phase.Owner {
		if kind == "ready" {
			return errors.New("not ready: phase is not owner")
		}
		return nil
	}
	data, err := os.ReadFile(runtimeStatusPath) //nolint:gosec // probeStatus receives the fixed container status path; the parameter isolates tests.
	if err != nil {
		return fmt.Errorf("read gateway status: %w", err)
	}
	var status struct {
		PID          int    `json:"pid"`
		StartTime    *int64 `json:"start_time"`
		UpdatedAt    string `json:"updated_at"`
		GatewayState string `json:"gateway_state"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return fmt.Errorf("decode gateway status: %w", err)
	}
	if status.PID <= 0 || status.StartTime == nil || *status.StartTime <= 0 {
		return errors.New("gateway process identity is unavailable")
	}
	processStart, err := readProcessStartTime(status.PID)
	if err != nil {
		return fmt.Errorf("read gateway process start time: %w", err)
	}
	containerStart, err := readProcessStartTime(1)
	if err != nil {
		return fmt.Errorf("read container start time: %w", err)
	}
	if processStart != *status.StartTime || processStart < containerStart {
		return errors.New("gateway process identity does not match this container")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, status.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parse gateway update time: %w", err)
	}
	if updatedAt.After(now) || now.Sub(updatedAt) > statusStaleAfter {
		return errors.New("gateway status is stale")
	}
	if status.GatewayState == "startup_failed" {
		return errors.New("gateway process is not healthy")
	}
	return nil
}

// readProcessStartTime reads Linux /proc stat field 22, Hermes' Linux fingerprint.
func readProcessStartTime(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	_, commandAndFields, found := bytes.CutLast(data, []byte(")"))
	if !found {
		return 0, errors.New("process stat has no command terminator")
	}
	fields := strings.Fields(string(commandAndFields))
	if len(fields) <= 19 {
		return 0, errors.New("process stat has no start time")
	}
	startTime, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return startTime, nil
}
