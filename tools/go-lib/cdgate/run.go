package cdgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

// CDSkipExitCode is the exit code of a gate skip. The pipeline runner sets it
// as the deploy step's skip_exit_code, so Woodpecker ends the step skipped. It
// must equal CD_SKIP_EXIT_CODE in the CD wrapper script.
const CDSkipExitCode = 79

// Run holds everything one CD run needs. Each external effect is a field, so
// tests substitute fakes and no test touches a cluster or the API.
type Run struct {
	// Gate returns the decision. An invalid CD API context blocks the deploy;
	// other gate errors fail open and deploy.
	Gate func(ctx context.Context) (Verdict, error)
	// Skip records a gate verdict. A skip exits CDSkipExitCode, not the deploy's code.
	Skip func(Verdict)
	// Lease is the Lease to hold, or nil to run without one.
	Lease *LeaseConfig
	// PrepareLease, when set, builds the lease instead of using Lease.
	PrepareLease func() (*LeaseConfig, error)
	Exec         Executor
	// Fence, when set, fences a Lease-taking deploy step through the release
	// queue. It runs under the Lease, before the deploy command.
	Fence func(ctx context.Context) (bool, error)
	// PublishFence, when set, fences a step without a Lease before the gate runs.
	// It reports whether this pipeline is the newest release.
	PublishFence func(ctx context.Context) (bool, error)
	// PulumiStacks contains registered Pulumi project names.
	PulumiStacks map[string]bool
	Clock        Clock
	Sleep        Sleeper
	// Bound caps each in-flight renewal at the loss deadline. Nil means WallBound.
	Bound Bound
	Log   io.Writer
}

// RunCD runs one CD target. A skip verdict returns CDSkipExitCode without
// touching the Lease, so Woodpecker's skip_exit_code ends the step skipped.
// A lease setup failure returns 1. A held Lease runs the deploy inside the
// acquire, renew and release lifecycle. Without a Lease the deploy runs
// directly.
func RunCD(ctx context.Context, target string, r Run) int {
	if r.Log != nil {
		r.Log = &lockedWriter{w: r.Log}
	}
	if r.PublishFence != nil {
		newest, err := r.PublishFence(ctx)
		if err != nil {
			logf(r.Log, "cd-release: newest-release check failed: %v", err)
			return 1
		}
		if !newest {
			logf(r.Log, "cd-release: pipeline is not the newest release; skipping")
			if r.Skip != nil {
				r.Skip(Verdict{Reason: ReasonNotNewestRelease})
			}
			return ReleaseFenceExitCode
		}
	}
	verdict, err := r.Gate(ctx)
	if errors.Is(err, woodpecker.ErrInvalidAPIContext) {
		logf(r.Log, "cd-gate: blocked (gate error: %v)", err)
		return 1
	}
	if err != nil {
		logf(r.Log, "cd-gate: deploy (gate error: %v)", err)
		verdict = Verdict{Deploy: true}
	}
	if !verdict.Deploy {
		logSkip(r.Log, verdict)
		if r.Skip != nil {
			r.Skip(verdict)
		}
		return CDSkipExitCode
	}
	logf(r.Log, "cd-gate: deploy")

	lease := r.Lease
	if r.PrepareLease != nil {
		prepared, err := r.PrepareLease()
		if errors.Is(err, kubeserviceaccount.ErrNotKubernetes) {
			err = nil
		}
		if err != nil {
			logf(r.Log, "cd-lease: setup failed: %v", err)
			return 1
		}
		lease = prepared
		if lease == nil {
			logf(r.Log, "cd-lease: skipped (KUBERNETES_SERVICE_HOST unset)")
		}
	}
	if lease == nil {
		if r.Fence != nil {
			logf(r.Log, "cd-release: fence needs the Lease; not deploying")
			return 1
		}
		code, err := Deploy(ctx, target, r.PulumiStacks, r.Exec, r.Sleep)
		if err != nil {
			logf(r.Log, "cd: deploy failed: %v", err)
			return 1
		}
		return deployExit(code)
	}
	return runWithLease(ctx, target, lease, r)
}

// runWithLease acquires the Lease, renews it while the deploy runs, and
// releases it after. An acquire failure returns 1 without releasing. A failed
// release fails an otherwise green step, because a held Lease would stall the
// next deploy for the full duration.
func runWithLease(ctx context.Context, target string, lease *LeaseConfig, r Run) int {
	acquiredAt, err := AcquireLease(ctx, lease, r.Clock, r.Sleep)
	if err != nil {
		logf(r.Log, "cd-lease: acquire failed: %v", err)
		return 1
	}

	// Renewal and release must outlive a stop: the deploy group may still be
	// running, and the release has to land. Each one ends on its own path.
	durable := context.WithoutCancel(ctx)
	renewCtx, stopRenewal := context.WithCancel(durable)
	renewDone := make(chan LeaseRenewal, 1)
	lost := make(chan struct{})
	go func() {
		bound := r.Bound
		if bound == nil {
			bound = WallBound
		}
		res := RenewUntilStopped(renewCtx, lease, acquiredAt, r.Clock, r.Sleep, bound, func(err error) {
			logf(r.Log, "cd-lease: renewal failed: %v", err)
		})
		renewDone <- res
		if res.Lost {
			close(lost)
		}
	}()

	deployCtx, cancelDeploy := context.WithCancel(ctx)
	defer cancelDeploy()
	watcherDone := make(chan struct{})
	// A lost Lease stops the deploy: no other holder may run beside it.
	go func() {
		defer close(watcherDone)
		select {
		case <-lost:
			logf(r.Log, "cd-lease: lost; stopping deploy")
			cancelDeploy()
		case <-deployCtx.Done():
		}
	}()
	stopDeployWatcher := func() {
		cancelDeploy()
		<-watcherDone
	}

	// release clears the holder. A failed release turns a green step red,
	// because a held Lease stalls the next deploy for the full duration.
	release := func(code int) int {
		if releaseErr := ReleaseLease(durable, lease, r.Clock, r.Sleep); releaseErr != nil {
			logf(r.Log, "cd-lease: release failed: %v", releaseErr)
			if code == 0 || code == CDSkipExitCode {
				return 1
			}
		}
		return code
	}

	if code, done := recheckUnderLease(deployCtx, r, lost); done {
		stopRenewal()
		<-renewDone
		stopDeployWatcher()
		return release(code)
	}
	if r.Fence != nil {
		current, err := r.Fence(deployCtx)
		if err != nil || !current {
			code := 0
			if err != nil {
				logf(r.Log, "cd-release: fence failed: %v", err)
				code = 1
			} else {
				logf(r.Log, "cd-release: not the current release; not deploying")
				if r.Skip != nil {
					r.Skip(Verdict{Reason: ReasonNotCurrentRelease})
				}
			}
			stopRenewal()
			<-renewDone
			stopDeployWatcher()
			return release(code)
		}
	}

	code, err := deployWhileHeld(deployCtx, target, r)
	if stuck, ok := errors.AsType[*DeployGroupStuckError](err); ok {
		// A surviving group could still apply, so renewal keeps the Lease held
		// until the group exits. A lost Lease lets it expire instead.
		logf(r.Log, "cd-lease: deploy group still running; holding the Lease until it exits")
		select {
		case <-stuck.Exited:
		case <-lost:
		}
		stopRenewal()
		<-renewDone
		stopDeployWatcher()
		select {
		case <-lost:
			return 1
		default:
		}
		return release(1)
	}
	if err != nil {
		logf(r.Log, "cd: deploy failed: %v", err)
		code = 1
	}

	stopRenewal()
	<-renewDone
	stopDeployWatcher()
	return release(deployExit(code))
}

// deployWhileHeld runs the deploy. A cancel from the lease-loss watcher
// stops it, and the step then returns 1.
func deployWhileHeld(ctx context.Context, target string, r Run) (int, error) {
	code, err := Deploy(ctx, target, r.PulumiStacks, r.Exec, r.Sleep)
	if ctx.Err() != nil {
		return 1, err
	}
	return code, err
}

// recheckUnderLease re-runs the gate while the Lease is held, so a newer covering
// pipeline that landed during acquire still skips the deploy. It reports done
// when the deploy must not run: the Lease was lost first, the gate says skip, or
// the CD API context is invalid. A gate error fails open and deploys.
func recheckUnderLease(ctx context.Context, r Run, lost <-chan struct{}) (int, bool) {
	type result struct {
		verdict Verdict
		err     error
	}
	gated := make(chan result, 1)
	go func() {
		v, err := r.Gate(ctx)
		gated <- result{v, err}
	}()
	select {
	case <-lost:
		logf(r.Log, "cd-lease: lost before deploy; not deploying")
		return 1, true
	case <-ctx.Done():
		select {
		case <-lost:
			logf(r.Log, "cd-lease: lost before deploy; not deploying")
		default:
			logf(r.Log, "cd-lease: stopped by signal; not deploying")
		}
		return 1, true
	case g := <-gated:
		if errors.Is(g.err, woodpecker.ErrInvalidAPIContext) {
			logf(r.Log, "cd-gate: blocked (gate error: %v)", g.err)
			return 1, true
		}
		if g.err != nil {
			logf(r.Log, "cd-gate: deploy (gate error: %v)", g.err)
			return 0, false
		}
		if !g.verdict.Deploy {
			logSkip(r.Log, g.verdict)
			if r.Skip != nil {
				r.Skip(g.verdict)
			}
			return CDSkipExitCode, true
		}
		return 0, false
	}
}

// deployExit keeps a deploy that exits CDSkipExitCode from reading as a
// skipped step: only the gate may skip.
func deployExit(code int) int {
	if code == CDSkipExitCode {
		return 1
	}
	return code
}

// logSkip records a skip verdict. A superseded skip names the pipeline that
// superseded it, on both the pre-lease and the under-lease recheck paths.
func logSkip(w io.Writer, v Verdict) {
	if v.Reason == ReasonSuperseded {
		logf(w, "cd-gate: skipped superseded %d", v.By)
		return
	}
	logf(w, "cd-gate: skipped %s", v.Reason)
}

// lockedWriter serializes writes from the concurrent lease renewer and deploy watcher.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	if _, err := fmt.Fprintf(w, format+"\n", args...); err != nil {
		return
	}
}
