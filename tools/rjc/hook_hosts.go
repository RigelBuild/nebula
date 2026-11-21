package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// minHostFreeGiB is the least free space on the pool disk ($HOME) a host needs to take a hook run.
const minHostFreeGiB = 15

// hostStatus is one host's load, free space, and free gate slots, read before a run.
type hostStatus struct {
	nproc         int
	load1         float64
	freeGiB       float64
	freeSlots     int
	queuePosition int
	queueTotal    int
}

// hostProbe reads one host's status. The real probe runs over ssh; tests inject a fake.
type hostProbe func(ctx context.Context, host string) (hostStatus, error)

// hostPick is the outcome of probing every host in the list.
type hostPick struct {
	chosen        string
	queuePosition int
	queueTotal    int
	// report has one line per host, in list order, for the run log.
	report []string
}

// splitHosts splits a comma-separated RJC_HOOK_SSH value. Empty items are dropped.
func splitHosts(value string) []string {
	var hosts []string
	for item := range strings.SplitSeq(value, ",") {
		if host := strings.TrimSpace(item); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// errNoFreeSlot reports that every qualifying host has all its gate slots busy. The caller queues and re-picks.
var errNoFreeSlot = errors.New("every qualifying host has all its gate slots busy")

// pickHost probes every host in parallel. Unreachable hosts and hosts below minHostFreeGiB are dropped.
// Among the rest it prefers a host with a free gate slot, then the lowest load per CPU. Ties keep list order.
// When hosts qualify but none has a free slot, it returns errNoFreeSlot with the per-host report.
func pickHost(ctx context.Context, hosts []string, probe hostProbe) (hostPick, error) {
	type result struct {
		status hostStatus
		err    error
	}
	results := make([]result, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Go(func() {
			status, err := probe(ctx, host)
			results[i] = result{status: status, err: err}
		})
	}
	wg.Wait()

	var pick hostPick
	var reasons []string
	best := -1
	bestLoad := math.Inf(1)
	waiting := -1
	waitingLoad := math.Inf(1)
	qualifying := 0
	for i, host := range hosts {
		res := results[i]
		switch {
		case res.err != nil:
			pick.report = append(pick.report, fmt.Sprintf("%s: unreachable (%v)", host, res.err))
			reasons = append(reasons, host+": unreachable")
		case res.status.freeGiB < minHostFreeGiB:
			pick.report = append(pick.report, fmt.Sprintf("%s: skipped, %.1f GiB free", host, res.status.freeGiB))
			reasons = append(reasons, fmt.Sprintf("%s: %.1f GiB free", host, res.status.freeGiB))
		default:
			qualifying++
			load := res.status.load1 / float64(res.status.nproc)
			pick.report = append(pick.report, fmt.Sprintf("%s: load %.2f, %.1f GiB free, %d slot(s) free", host, load, res.status.freeGiB, res.status.freeSlots))
			if load < waitingLoad {
				waiting, waitingLoad = i, load
			}
			eligible := res.status.freeSlots > 0 && (res.status.queueTotal == 0 || (res.status.queuePosition > 0 && res.status.queuePosition <= res.status.freeSlots))
			if eligible && load < bestLoad {
				best, bestLoad = i, load
			}
		}
	}
	if qualifying == 0 {
		return pick, fmt.Errorf("no host qualifies: %s", strings.Join(reasons, "; "))
	}
	if best < 0 {
		pick.chosen = hosts[waiting]
		pick.queuePosition, pick.queueTotal = results[waiting].status.queuePosition, results[waiting].status.queueTotal
		return pick, errNoFreeSlot
	}
	pick.chosen = hosts[best]
	pick.queuePosition, pick.queueTotal = results[best].status.queuePosition, results[best].status.queueTotal
	return pick, nil
}

// parseHostStatus reads CPU, load, free space, slots, queue position, and queue count.
func parseHostStatus(out string) (hostStatus, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 4 {
		return hostStatus{}, fmt.Errorf("probe output has %d lines, want 4", len(lines))
	}
	nproc, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || nproc <= 0 {
		return hostStatus{}, fmt.Errorf("invalid nproc %q", lines[0])
	}
	load, err := strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	if err != nil {
		return hostStatus{}, fmt.Errorf("invalid load average %q", lines[1])
	}
	freeKiB, err := strconv.ParseFloat(strings.TrimSpace(lines[2]), 64)
	if err != nil {
		return hostStatus{}, fmt.Errorf("invalid free space %q", lines[2])
	}
	slots, err := strconv.Atoi(strings.TrimSpace(lines[3]))
	if err != nil || slots < 0 {
		return hostStatus{}, fmt.Errorf("invalid free slot count %q", lines[3])
	}
	status := hostStatus{nproc: nproc, load1: load, freeGiB: freeKiB / (1 << 20), freeSlots: slots}
	if len(lines) >= 6 {
		status.queuePosition, err = strconv.Atoi(strings.TrimSpace(lines[4]))
		if err != nil || status.queuePosition < 0 {
			return hostStatus{}, fmt.Errorf("invalid queue position %q", lines[4])
		}
		status.queueTotal, err = strconv.Atoi(strings.TrimSpace(lines[5]))
		if err != nil || status.queueTotal < 0 {
			return hostStatus{}, fmt.Errorf("invalid queue total %q", lines[5])
		}
	}
	return status, nil
}

// sshHostProbeScript prints six status lines. Arguments are slot count and client ticket ID.
// A free slot count is a snapshot; the run rechecks admission under the queue lock.
var sshHostProbeScript = `nproc
read -r load _ < /proc/loadavg
echo "$load"
df -Pk "$HOME" | sed -n 2p | tr -s ' ' | cut -d' ' -f4
slots_dir="$HOME/rjc-hook/slots"
total="$1"
host_slots=${RJC_HOOK_SSH_HOST_SLOTS:-}
case "$host_slots" in
  ''|*[!0-9]*) ;;
  *) if [ "$host_slots" -gt 0 ] && [ "$host_slots" -lt "$total" ]; then total=$host_slots; fi ;;
esac
free=0
mkdir -p "$slots_dir"
i=1
while [ "$i" -le "$total" ]; do
  if flock -n "$slots_dir/s$i" true 2>/dev/null; then free=$((free+1)); fi
  i=$((i+1))
done
echo "$free"
ticket=${2:-}
queue_dir="$HOME/rjc-hook/queue"
mkdir -p "$queue_dir"
exec 8>"$queue_dir/.lock"
flock 8
` + sshQueueScan + `if [ -n "$ticket" ] && { [ -n "$queue_file" ] || [ "$free" -eq 0 ] || [ "$n" -gt 0 ]; }; then
  if [ -z "$queue_file" ]; then
    stamp=$(date +%s%N)
    queue_file="$queue_dir/$(printf '%020d' "$stamp")-$ticket"
  fi
  touch "$queue_file"
fi
` + sshQueueScan + `echo "$pos"
echo "$n"
flock -u 8
`
