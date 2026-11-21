package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Read the remote exit status from ssh.
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cacheName returns the remote cache path under ~/rjc-hook, as owner/repo. The root must not sit under a
// directory named .cache: some formatters and parity tests skip trees with a .cache path component.
func (r sshHookRunner) cacheName() (string, error) {
	owner, repo, ok := strings.Cut(r.repo.profile.Name, "/")
	if !ok || !sshCachePart.MatchString(owner) || !sshCachePart.MatchString(repo) {
		return "", fmt.Errorf("invalid repo name %q for the ssh host cache", r.repo.profile.Name)
	}
	return owner + "/" + repo, nil
}

// haveCommits returns the local commits the host cache already holds, read from its hook refs.
func (r sshHookRunner) haveCommits(ctx context.Context, log io.Writer) ([]string, error) {
	name, err := r.cacheName()
	if err != nil {
		return nil, err
	}
	var listing bytes.Buffer
	if err := r.ssh(ctx, "sh -c "+shQuote(sshProbeScript)+" rjc-probe "+shQuote(name), nil, &listing, log); err != nil {
		return nil, fmt.Errorf("probe ssh host %s: %w", r.host, err)
	}
	var shas []string
	for line := range strings.SplitSeq(strings.TrimSpace(listing.String()), "\n") {
		if line == "" {
			continue
		}
		if !objectID.MatchString(line) {
			return nil, fmt.Errorf("invalid host cache probe line %q", line)
		}
		shas = append(shas, line)
	}
	if len(shas) == 0 {
		return nil, nil
	}
	check, err := r.repo.command(ctx, nil, []byte(strings.Join(shas, "\n")+"\n"), "git", "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	var have []string
	for line := range strings.SplitSeq(string(check), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[1] == "commit" {
			have = append(have, fields[0])
		}
	}
	return have, nil
}

// writePayload writes the bundle the host needs. An empty file means the host already holds head.
func (r sshHookRunner) writePayload(ctx context.Context, payload, ref, head string, have []string) error {
	if len(have) == 0 {
		return r.makeBundle(ctx, payload, ref, nil)
	}
	count, err := r.repo.command(ctx, nil, nil, "git", append([]string{"rev-list", "--count", head, "--not"}, have...)...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(count)) == "0" {
		return os.WriteFile(payload, nil, 0o600)
	}
	return r.makeBundle(ctx, payload, ref, have)
}

// makeBundle writes a bundle of ref. Commits reachable from exclude are omitted.
func (r sshHookRunner) makeBundle(ctx context.Context, payload, ref string, exclude []string) error {
	args := []string{"bundle", "create", payload, ref}
	if len(exclude) != 0 {
		args = append(append(args, "--not"), exclude...)
	}
	_, err := r.repo.command(ctx, nil, nil, "git", args...)
	return err
}

// ssh runs one remote command. A nil stdin sends nothing.
func (r sshHookRunner) ssh(ctx context.Context, remote string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := r.sshCommand(ctx, remote, stdout, stderr)
	cmd.Stdin = stdin
	return cmd.Run()
}

// sshCommand builds the ssh command for remote with the shared options.
func (r sshHookRunner) sshCommand(ctx context.Context, remote string, stdout, stderr io.Writer) *exec.Cmd {
	args := []string{"-o", sshBatchMode, "-o", sshConnectTimeout, "-o", sshKeepAlive, "-o", sshKeepAliveCount}
	if r.identity != "" {
		args = append(args, "-i", r.identity)
	}
	args = append(args, "--", r.host, remote)
	cmd := r.repo.execCommand(ctx, "ssh", args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = r.repo.root, r.repo.childEnv(nil), stdout, stderr
	return cmd
}

// sshFramed sends "<size>\n" and payload, then keeps ssh stdin open until the remote exits or ctx ends ssh.
// The remote guard reads the frame and treats a later EOF as a dead client.
func (r sshHookRunner) sshFramed(ctx context.Context, remote, payload string, stdout, stderr io.Writer) error {
	in, err := os.Open(payload) //nolint:gosec // The payload path is in a private temporary directory.
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // Read-only file; a close error cannot lose data.
	info, err := in.Stat()
	if err != nil {
		return err
	}
	cmd := r.sshCommand(ctx, remote, stdout, stderr)
	// After a cancel, stop waiting on output pipes a remote child might still hold, and close stdin so the guard sees EOF.
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sent := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintf(stdin, "%d\n", info.Size())
		if err == nil {
			_, err = io.Copy(stdin, in)
		}
		sent <- err
	}()
	// Wait closes stdin only after ssh exits, so the remote sees EOF only when the client is gone.
	runErr := cmd.Wait()
	if err := <-sent; err != nil && runErr == nil {
		return fmt.Errorf("send payload: %w", err)
	}
	return runErr
}

// transfer streams payload to the host and returns the remote result tar.
// Exit 75 means the host lacks a prerequisite, so the caller retries with a full bundle.
func (r sshHookRunner) transfer(ctx context.Context, payload, base, head, kind, config string, log io.Writer) ([]byte, error) {
	// The remote shell splits RJC_HOOK_SSH_SHELL on whitespace and supports no quoting, so a quote can only break it.
	if shell := envLookup(r.repo.env, "RJC_HOOK_SSH_SHELL"); strings.ContainsAny(shell, `'"`) {
		return nil, toolFailure("hook-ssh", errors.New("RJC_HOOK_SSH_SHELL is split on whitespace and supports no quoting: remove the ' and \" characters"))
	}
	name, err := r.cacheName()
	if err != nil {
		return nil, err
	}
	slots, err := r.hookSlots()
	if err != nil {
		return nil, err
	}
	timeout, err := r.hookTimeout()
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		// Keep a margin so the remote TERM lands before the client deadline kills ssh.
		timeout = max(time.Until(deadline)-sshDeadlineMargin, time.Millisecond)
	}
	reports := "0"
	if r.repo.profile.HookReports {
		reports = "1"
	}
	limit := shQuote(strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64) + "s")
	owner, repo, _ := strings.Cut(name, "/")
	remote := "RJC_HOOK_TICKET=" + shQuote(r.ticket) + " RJC_HOOK_SSH_SHELL=" + shQuote(envLookup(r.repo.env, "RJC_HOOK_SSH_SHELL")) + " sh -c " + shQuote(sshHookGuard) + " rjc-guard " + limit + " " + shQuote(sshHookScript()) + " rjc-hook " + shQuote(base) + " " + shQuote(head) + " " + reports + " " + shQuote(owner) + " " + shQuote(repo) + " " + strconv.Itoa(slots) + " " + shQuote(kind) + " " + shQuote(config) + " " + limit
	var result bytes.Buffer
	start := time.Now()
	runErr := r.sshFramed(ctx, remote, payload, &result, log)
	if runErr != nil {
		if exit, ok := errors.AsType[*exec.ExitError](runErr); ok && exit.ExitCode() == sshNeedFullBundle {
			return nil, errNeedFullBundle
		}
		if exit, ok := errors.AsType[*exec.ExitError](runErr); ok && exit.ExitCode() == sshMissingConfig {
			return nil, errMissingConfig
		}
		if exit, ok := errors.AsType[*exec.ExitError](runErr); ok && exit.ExitCode() == sshMissingRunner {
			return nil, errMissingRunner
		}
		if exit, ok := errors.AsType[*exec.ExitError](runErr); ok && exit.ExitCode() == sshSlotBusy {
			return nil, errSlotBusy
		}
		// 137 is a timeout only once the remote limit has passed; before that it is a step killed by other means.
		if exit, ok := errors.AsType[*exec.ExitError](runErr); ok && (exit.ExitCode() == sshRemoteTimeout || exit.ExitCode() == timeoutExit || (exit.ExitCode() == timeoutKilledExit && time.Since(start) >= timeout)) {
			return nil, toolFailure("hook-ssh-timeout", fmt.Errorf("remote hook exceeded %s", timeout))
		}
		return nil, fmt.Errorf("ssh %s: %w", r.host, runErr)
	}
	return result.Bytes(), nil
}

// readSSHResults unpacks the remote result tar into dir. It accepts only the files this run expects.
func readSSHResults(data []byte, dir string, reports bool) error {
	want := map[string]bool{"exit": true}
	if reports {
		want["moon.json"], want["gates.json"] = true, true
	}
	seen := map[string]bool{}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid remote result: %w", err)
		}
		if !want[header.Name] || seen[header.Name] || header.Size > maxSSHResult {
			return fmt.Errorf("unexpected remote result %q", header.Name)
		}
		seen[header.Name] = true
		file, err := os.Create(filepath.Join(dir, header.Name)) //nolint:gosec // Names are checked against a fixed set.
		if err != nil {
			return err
		}
		_, err = io.Copy(file, io.LimitReader(reader, header.Size))
		if err = errors.Join(err, file.Close()); err != nil {
			return err
		}
	}
	for name := range want {
		if !seen[name] {
			return fmt.Errorf("missing remote result %q", name)
		}
	}
	return nil
}
