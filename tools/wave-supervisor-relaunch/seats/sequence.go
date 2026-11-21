package seats

import (
	"context"
	"fmt"
	"time"
)

// sequence is one seat's relaunch. Each step is a callback, so the order is testable.
type sequence struct {
	idleNow  func(context.Context, string) (bool, error)
	stop     func(context.Context, string) error
	present  func(context.Context, string) (present, complete bool, err error)
	restart  func(context.Context, string) (int, error)
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	deadline time.Duration
	poll     time.Duration
	seatCLI  string
}

// relaunch stops an idle seat, waits until it is absent, then restarts it through
// `wave-seats restart`, which bumps the generation so reconcile spawns a fresh session. A seat
// that went busy is left alone with no command. A failed stop or wait never restarts.
func (s sequence) relaunch(ctx context.Context, name string) (string, error) {
	idle, err := s.idleNow(ctx, name)
	if err != nil {
		return "", fmt.Errorf("idle re-check: %w", err)
	}
	if !idle {
		return outcomeWorking, nil
	}
	if err := s.stop(ctx, name); err != nil {
		return "", fmt.Errorf("stop: %w", err)
	}
	if err := s.waitAbsent(ctx, name); err != nil {
		return "", err
	}
	code, err := s.restart(ctx, name)
	if err != nil {
		return "", fmt.Errorf("%s restart: %w", s.seatCLI, err)
	}
	if code != 0 {
		return "", fmt.Errorf("%s restart exited %d", s.seatCLI, code)
	}
	return outcomeRelaunched, nil
}

// waitAbsent returns once the seat is absent by the test `wave-seats restart` applies. The clock
// starts here, after stop, so a slow re-check or stop cannot spend the budget. The deadline is
// wall clock, so a slow probe spends it. A failed read or unavailable manager status holds the
// wait and is named at the deadline.
func (s sequence) waitAbsent(ctx context.Context, name string) error {
	started := s.now()
	for {
		present, complete, err := s.present(ctx, name)
		if err == nil && complete && !present {
			return nil
		}
		if waited := s.now().Sub(started); waited >= s.deadline {
			switch {
			case err != nil:
				return fmt.Errorf("liveness unknown after stop (%v): %w", waited, err)
			case !complete:
				return fmt.Errorf("manager status unavailable after stop (%v); liveness unknown", waited)
			default:
				return fmt.Errorf("seat %s still present after stop (%v)", name, waited)
			}
		}
		if err := s.sleep(ctx, s.poll); err != nil {
			return err
		}
	}
}

// step relaunches at most one idle seat over the threshold, largest first, and logs each seat
// over it. The root seat is never chosen. A hold skips every seat. A failed relaunch uses the
// run's slot; a seat that went busy at the re-check does not, so the next seat is tried.
func step(ctx context.Context, root string, seats []Candidate, threshold int64, hold bool, relaunch func(context.Context, string) (string, error), log func(string)) {
	relaunched := false
	for _, seat := range ordered(root, seats) {
		switch {
		case hold:
			log(line(seat, "skipped:hold"))
			continue
		case seat.MemoryBytes < threshold:
			continue
		case !seat.Idle:
			log(line(seat, outcomeWorking))
			continue
		case relaunched:
			log(line(seat, "skipped:one-per-run"))
			continue
		}
		outcome, err := relaunch(ctx, seat.Name)
		if err != nil {
			log(fmt.Sprintf("%s reason=%q", line(seat, "failed"), err.Error()))
			relaunched = true
			continue
		}
		log(line(seat, outcome))
		relaunched = relaunched || outcome == outcomeRelaunched
	}
}

func line(seat Candidate, outcome string) string {
	return fmt.Sprintf("wave-supervisor-relaunch: seat outcome=%s seat=%s anon_bytes=%d", outcome, seat.Name, seat.MemoryBytes)
}
