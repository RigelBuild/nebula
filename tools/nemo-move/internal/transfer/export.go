package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	manifestName = ".nemo-move-manifest.json"
	stageSuffix  = "/.local/state/nemo-move-staging"
	dataRoot     = "opt/data"
	pageRoot     = "control/nemo-page"
)

// Options names the source account whose home is exported and rewritten.
type Options struct {
	// SourceHome is the absolute source home rewritten to /opt/data.
	SourceHome string
	// MacUser is the SSH user that owns SourceHome.
	MacUser string
}

// Validate reports whether the options are complete.
func (o Options) Validate() error {
	if err := o.validateSourceHome(); err != nil {
		return err
	}
	if !safeTargetPart(o.MacUser) {
		return fmt.Errorf("invalid source SSH user %q", o.MacUser)
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
	if _, stageErr := runRemote(ctx, target, sshPath, remote, "/usr/bin/sudo -n /bin/sh -c "+ShellQuote(stageCredentialsScript(opts))); stageErr != nil {
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
	if _, err := Inventory(ctx, target, sshPath, binary, remote); err != nil {
		return fmt.Errorf("inventory durable Copy rows: %w", err)
	}
	path, err := EnsureMacBinary(ctx, remote, sshPath, target, binary)
	if err != nil {
		return err
	}
	source, err := remote.StreamReader(ctx, sshPath, "-o", "BatchMode=yes", target, "\"$HOME/\""+ShellQuote(path)+" mac-side export")
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

func stageCredentialsScript(opts Options) string {
	return `set -eu
base=` + ShellQuote(opts.stageRoot()) + `
owner=` + ShellQuote(opts.MacUser+":staff") + `
parent=${base%/*}
temp="$parent/.nemo-move-staging.$$"
/bin/mkdir -p "$parent"
/bin/rm -rf "$temp"
/bin/mkdir -p "$temp"
/bin/chmod 700 "$temp"
for account in workspace personal; do
  source="/var/lib/nemo-google-mcp/$account/credentials"
  /bin/test -d "$source"
  /bin/mkdir -p "$temp/$account"
  /bin/cp -R "$source" "$temp/$account/"
  /usr/sbin/chown -R "$owner" "$temp/$account/credentials"
  /usr/bin/find "$temp/$account/credentials" -type d -exec /bin/chmod 700 {} \;
  /usr/bin/find "$temp/$account/credentials" -type f -exec /bin/chmod 600 {} \;
done
/usr/sbin/chown -R "$owner" "$temp"
/bin/chmod 700 "$temp"
/bin/rm -rf "$base.old"
if [ -e "$base" ]; then /bin/mv "$base" "$base.old"; fi
/bin/mv "$temp" "$base"
/bin/rm -rf "$base.old"
/usr/bin/touch "$base"`
}

func stageCredentialsCommand(opts Options) string {
	return "sudo /bin/sh -c " + ShellQuote(stageCredentialsScript(opts))
}

func stagePresenceCommand(opts Options) string {
	script := "if [ -d " + ShellQuote(opts.stageRoot()) + " ]; then printf exists; else printf absent; fi"
	return "/bin/sh -c " + ShellQuote(script)
}

func stageFreshnessCommand(opts Options) string {
	script := "quiet=" + ShellQuote(opts.SourceHome+"/.local/state/nemo-move/last-quiet") + "; stage=" + ShellQuote(opts.stageRoot()) + "; if [ -f \"$quiet\" ] && [ -n \"$(/usr/bin/find \"$stage\" -newer \"$quiet\" -print -quit)\" ]; then printf fresh; fi"
	return "/bin/sh -c " + ShellQuote(script)
}
