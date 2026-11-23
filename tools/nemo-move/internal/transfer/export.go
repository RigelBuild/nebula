package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
)

const (
	manifestName = ".nemo-move-manifest.json"
	stageSuffix  = "/" + layout.StagingDir
	dataRoot     = "opt/data"
	pageRoot     = "control/nemo-page"
)

// Options names the source account whose home is exported and rewritten, and
// the deployment layout that says what is copied and where it lands.
type Options struct {
	// SourceHome is the absolute source home rewritten to /opt/data.
	SourceHome string
	// MacUser is the SSH user that owns SourceHome.
	MacUser string
	// Layout is the durable-state table from --inventory.
	Layout layout.Layout
}

// Validate reports whether the options are complete.
func (o Options) Validate() error {
	if err := o.validateSourceHome(); err != nil {
		return err
	}
	if !safeTargetPart(o.MacUser) {
		return fmt.Errorf("invalid source SSH user %q", o.MacUser)
	}
	if err := o.Layout.Validate(); err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	return nil
}

func (o Options) validateSourceHome() error {
	if !strings.HasPrefix(o.SourceHome, "/") || o.SourceHome == "/" || strings.HasSuffix(o.SourceHome, "/") {
		return fmt.Errorf("source home must be an absolute path without a trailing slash, got %q", o.SourceHome)
	}
	return nil
}

func (o Options) stageRoot() string {
	return o.SourceHome + stageSuffix
}

// HomeCommand prints the remote login home without a trailing newline.
const HomeCommand = `printf %s "$HOME"`

// CheckSourceHome fails unless the remote $HOME of target equals opts.SourceHome.
func CheckSourceHome(ctx context.Context, opts Options, target, sshPath string, remote Remote) error {
	home, err := runRemote(ctx, target, sshPath, remote, HomeCommand)
	if err != nil {
		return fmt.Errorf("read remote home: %w", err)
	}
	if home != opts.SourceHome {
		return fmt.Errorf("remote home %q does not match source home %q", home, opts.SourceHome)
	}
	return nil
}

// Export streams a verified archive of the source home to stdout.
func Export(ctx context.Context, opts Options, target, sshPath, binary string, remote Remote, stdout, stderr io.Writer) error {
	return ExportToSpool(ctx, opts, target, sshPath, binary, "", remote, stdout, stderr)
}

// ExportToSpool is Export with an explicit spool directory.
func ExportToSpool(ctx context.Context, opts Options, target, sshPath, binary, spoolDir string, remote Remote, stdout, stderr io.Writer) (result error) {
	if err := opts.Validate(); err != nil {
		return err
	}
	user, err := splitTarget(target)
	if err != nil {
		return err
	}
	if user != opts.MacUser {
		return fmt.Errorf("mac export requires %s as the SSH user", opts.MacUser)
	}
	if remote == nil || stdout == nil || stderr == nil {
		return errors.New("export requires remote and output writers")
	}
	if err := CheckSourceHome(ctx, opts, target, sshPath, remote); err != nil {
		return err
	}
	if _, stageErr := runRemote(ctx, target, sshPath, remote, stagePipeline(opts)); stageErr != nil {
		stageState, err := runRemote(ctx, target, sshPath, remote, stagePresenceCommand(opts))
		if err != nil {
			return fmt.Errorf("check credential staging state: %w", err)
		}
		if strings.TrimSpace(stageState) != "exists" {
			_, writeErr := fmt.Fprintf(stderr, "sudo -n staging failed; run this root command interactively, then rerun nemo-move mac export:\n%s\n", stageCredentialsCommand(opts))
			return errors.Join(fmt.Errorf("cannot stage Google credentials: %w", stageErr), writeErr)
		}
		fresh, err := runRemote(ctx, target, sshPath, remote, stageFreshnessCommand(opts))
		if err != nil || strings.TrimSpace(fresh) != "fresh" {
			_, writeErr := fmt.Fprintf(stderr, "staging is absent or stale; run this root command interactively, then rerun nemo-move mac export:\n%s\n", stageCredentialsCommand(opts))
			return errors.Join(errors.New("credential staging is not fresh"), err, writeErr)
		}
	}
	if _, err := Inventory(ctx, target, sshPath, binary, remote, opts.Layout); err != nil {
		return fmt.Errorf("inventory durable Copy rows: %w", err)
	}
	path, err := EnsureMacBinary(ctx, remote, sshPath, target, binary)
	if err != nil {
		return err
	}
	layoutJSON, err := opts.Layout.JSON()
	if err != nil {
		return err
	}
	source, err := remote.StreamReader(ctx, sshPath, "-o", "BatchMode=yes", target, "\"$HOME/\""+ShellQuote(path)+" mac-side export "+ShellQuote(layoutJSON))
	if err != nil {
		return fmt.Errorf("start durable-state export: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			result = errors.Join(result, source.Close())
		}
	}()
	return writeFramedArchiveTo(source, stdout, spoolDir, func() error {
		closed = true
		return source.Close()
	})
}

// stageCredentialsScript is the root half of staging. It reads the Google
// source and streams the named accounts' credentials out as one tar archive on
// stdout. Root writes nothing in the user's tree: it creates, chowns, moves and
// removes no path there. The arguments are the Google source, then the account
// names, never shell text.
const stageCredentialsScript = `set -eu
source=$1
shift
for account in "$@"; do
  /bin/test -d "$source/$account/credentials"
  unreadable=$(/usr/bin/find "$source/$account/credentials" -type f ! -perm -u+r -print -quit)
  if [ -n "$unreadable" ]; then echo "unreadable credential $unreadable" >&2; exit 1; fi
done
cd "$source"
exec /usr/bin/tar -c "$@"
`

// stageUnpackScript is the user half of staging. It runs as the SSH user with no
// sudo. It refuses a symlinked parent, unpacks the archive file into a mktemp
// dir the user owns, sets the modes, and renames the tree into place. The
// arguments are the archive path, the staging path, then the account names.
const stageUnpackScript = `set -eu
archive=$1
shift
base=$1
shift
parent=${base%/*}
for dir in "$parent" "${parent%/*}" "${parent%/*/*}"; do
  if [ -L "$dir" ]; then echo "refusing symlinked directory $dir" >&2; exit 1; fi
done
if [ -L "$base" ]; then echo "refusing symlinked staging $base" >&2; exit 1; fi
/bin/mkdir -p "$parent"
temp=$(/usr/bin/mktemp -d "$parent/.nemo-move-staging.XXXXXX")
old=$(/usr/bin/mktemp -d "$parent/.nemo-move-old.XXXXXX")
trap '/bin/rm -rf "$temp"; /bin/rmdir "$old" 2>/dev/null || true' EXIT
/usr/bin/tar -x -C "$temp" -f "$archive"
for account in "$@"; do
  if [ -z "$(/bin/ls -A "$temp/$account/credentials" 2>/dev/null)" ]; then echo "no credentials for $account" >&2; exit 1; fi
  /usr/bin/find "$temp/$account" -type d -exec /bin/chmod 700 {} \;
  /usr/bin/find "$temp/$account" -type f -exec /bin/chmod 600 {} \;
done
/bin/chmod 700 "$temp"
if [ -e "$base" ]; then /bin/mv "$base" "$old/previous"; fi
if ! /bin/mv "$temp" "$base"; then
  if [ -e "$old/previous" ]; then /bin/mv "$old/previous" "$base"; fi
  exit 1
fi
trap - EXIT
/bin/rm -rf "$old"
/usr/bin/touch "$base"
`

// stageShell is the root command. It streams the accounts out as tar. Its
// inventory values are positional arguments: the Google source, then the account
// names. Each is quoted here, and none is spliced into the script text.
func stageShell(opts Options) string {
	args := append([]string{opts.Layout.Google.Source}, opts.Layout.Google.Accounts...)
	return "/bin/sh -c " + ShellQuote(stageCredentialsScript) + " sh " + shellJoin(args)
}

// stageUnpackShell is the user command that consumes the archive. It runs with
// no sudo. Its arguments are the archive path (the shell variable archive, set
// by stageRun), the staging path, then the account names.
func stageUnpackShell(opts Options) string {
	args := append([]string{opts.stageRoot()}, opts.Layout.Google.Accounts...)
	return "/bin/sh -c " + ShellQuote(stageUnpackScript) + " sh \"$archive\" " + shellJoin(args)
}

func stageCredentialsCommand(opts Options) string {
	return stageRun(opts, "sudo ")
}

// stageRun is the one staging command, used by the production run and the
// printed fallback so the two cannot drift. The user shell makes a 0600 archive
// in its own mktemp dir and removes that dir on every exit. Root streams into the
// archive; the unpack runs only if root exited 0. A root failure therefore
// leaves the old staging untouched.
func stageRun(opts Options, rootPrefix string) string {
	script := "umask 077; " +
		"parent=" + ShellQuote(filepath.Dir(opts.stageRoot())) + "; " +
		"/bin/mkdir -p \"$parent\" && " +
		"work=$(/usr/bin/mktemp -d \"$parent/.nemo-move-run.XXXXXX\") && " +
		"trap '/bin/rm -rf \"$work\"' EXIT && " +
		"archive=\"$work/stream.tar\" && : > \"$archive\" && " +
		rootPrefix + stageShell(opts) + " > \"$archive\" && " +
		stageUnpackShell(opts)
	return "/bin/bash -o pipefail -c " + ShellQuote(script)
}

// stagePipeline is the production staging command. Only the root stream runs
// under sudo, and the user side runs as the SSH user.
func stagePipeline(opts Options) string {
	return stageRun(opts, "/usr/bin/sudo -n ")
}

func stagePresenceCommand(opts Options) string {
	script := "if [ -d " + ShellQuote(opts.stageRoot()) + " ]; then printf exists; else printf absent; fi"
	return "/bin/sh -c " + ShellQuote(script)
}

func stageFreshnessCommand(opts Options) string {
	script := "quiet=" + ShellQuote(opts.SourceHome+"/.local/state/nemo-move/last-quiet") + "; stage=" + ShellQuote(opts.stageRoot()) + "; if [ -f \"$quiet\" ] && [ -n \"$(/usr/bin/find \"$stage\" -newer \"$quiet\" -print -quit)\" ]; then printf fresh; fi"
	return "/bin/sh -c " + ShellQuote(script)
}

// shellJoin quotes each word for a POSIX shell and joins them with spaces.
func shellJoin(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = ShellQuote(word)
	}
	return strings.Join(quoted, " ")
}
