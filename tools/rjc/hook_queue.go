package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os/exec" //nolint:depguard // Inspect the remote heartbeat exit status.
	"strconv"
	"sync"
	"time"
)

const sshBatchMode = "BatchMode=yes"
const sshConnectTimeout = "ConnectTimeout=10"

// sshKeepAlive and sshKeepAliveCount make ssh give up on a dead link within about a minute, so a cancelled submit
// does not wait on a silent remote. No tty is requested: a pty would rewrite the binary result stream.
const sshKeepAlive = "ServerAliveInterval=15"
const sshKeepAliveCount = "ServerAliveCountMax=4"

// sshQueueScan runs with the queue lock held. find uses the host clock for expiry.
var sshQueueScan = `LC_ALL=C; export LC_ALL
find "$queue_dir" -maxdepth 1 -type f ! -name '.*' ! -newermt '` + strconv.FormatInt(int64(3*slotPollInterval/time.Second), 10) + ` seconds ago' -delete
pos=0
n=0
queue_file=""
for path in "$queue_dir"/*; do
  [ -f "$path" ] || continue
  n=$((n + 1))
  case "${path##*/}" in *-"$ticket") [ -n "$ticket" ] && { pos=$n; queue_file=$path; } ;; esac
done
`

var sshQueueGate = `ticket=${RJC_HOOK_TICKET:-}
queue_dir="$HOME/rjc-hook/queue"
mkdir -p "$queue_dir"
exec 8>"$queue_dir/.lock"
flock 8
` + sshQueueScan + `free=0
export queue_file
for s in $(seq 1 "$slots"); do
  if flock -n "$slots_dir/s$s" true 2>/dev/null; then free=$((free + 1)); fi
done
if [ "$n" -gt 0 ] && { [ "$pos" -eq 0 ] || [ "$pos" -gt "$free" ]; }; then
  echo hook-ssh-slot-busy >&2; exit 77
fi
`

func newQueueTicket() string {
	return rand.Text()
}

type queueHosts struct {
	mu    sync.Mutex
	hosts map[string]bool
}

func (q *queueHosts) add(host string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.hosts == nil {
		q.hosts = make(map[string]bool)
	}
	q.hosts[host] = true
}

func (r sshHookRunner) cleanupTickets(ctx context.Context) { r.cleanupOtherTickets(ctx, "") }

func (r sshHookRunner) cleanupOtherTickets(ctx context.Context, keep string) {
	if r.queue == nil || r.repo == nil {
		return
	}
	r.queue.mu.Lock()
	hosts := make([]string, 0, len(r.queue.hosts))
	for host := range r.queue.hosts {
		if host != keep {
			hosts = append(hosts, host)
			delete(r.queue.hosts, host)
		}
	}
	r.queue.mu.Unlock()
	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Go(func() {
			args := []string{"-o", sshBatchMode, "-o", sshConnectTimeout}
			if r.identity != "" {
				args = append(args, "-i", r.identity)
			}
			args = append(args, "--", host, "sh -c "+shQuote(`for path in "$HOME/rjc-hook/queue/"*-"$1"; do [ -f "$path" ] && rm -f -- "$path"; done; :`)+" cleanup "+shQuote(r.ticket))
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			cmd := r.repo.execCommand(cleanupCtx, "ssh", args...)
			cmd.Dir, cmd.Env = r.repo.root, r.repo.childEnv(nil)
			if err := cmd.Run(); err != nil {
				slog.Warn("hook ticket cleanup failed", "host", host, "error", err)
			}
		})
	}
	wg.Wait()
}

// heartbeatTicket keeps the selected ticket live while preparing and uploading a bundle.
func (r sshHookRunner) heartbeatTicket(ctx context.Context, interval time.Duration) func() {
	if r.queue == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				args := []string{"-o", sshBatchMode, "-o", sshConnectTimeout}
				if r.identity != "" {
					args = append(args, "-i", r.identity)
				}
				args = append(args, "--", r.host, "sh -c "+shQuote(`found=0; for p in "$HOME/rjc-hook/queue/"*-"$1"; do [ -f "$p" ] || continue; touch -c "$p"; found=1; done; [ "$found" = 1 ] || exit 3`)+" heartbeat "+shQuote(r.ticket))
				cmd := r.repo.execCommand(ctx, "ssh", args...)
				cmd.Dir, cmd.Env = r.repo.root, r.repo.childEnv(nil)
				if err := cmd.Run(); err != nil && ctx.Err() == nil {
					if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 3 {
						return
					}
					slog.Warn("hook ticket heartbeat failed", "host", r.host, "error", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func queueProbeArgs(slots int, ticket string) string {
	return " rjc-probe " + strconv.Itoa(slots) + " " + shQuote(ticket)
}
