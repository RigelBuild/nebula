package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

func stopManagedSeats(ctx context.Context, names []string, space string, runner processRunner, live liveCheck) error {
	if len(names) == 0 {
		return nil
	}
	if runner == nil {
		return errors.New("stop managed seats: missing runner")
	}
	return stopManagedSeatsWithDeadline(ctx, names, space, runner, live, time.Duration(seatStopDeadlineMS)*time.Millisecond, context.WithTimeout)
}

func stopManagedSeatsWithDeadline(ctx context.Context, names []string, space string, runner processRunner, live liveCheck, deadline time.Duration, withDeadline func(context.Context, time.Duration) (context.Context, context.CancelFunc)) error {
	if len(names) == 0 {
		return nil
	}
	if runner == nil {
		return errors.New("stop managed seats: missing runner")
	}
	deadlineCtx, cancel := withDeadline(ctx, deadline)
	defer cancel()
	return stopManagedSeatsUntil(deadlineCtx, names, space, runner, live)
}

func stopManagedSeatsUntil(deadlineCtx context.Context, names []string, space string, runner processRunner, live liveCheck) error {
	if len(names) == 0 {
		return nil
	}
	if runner == nil {
		return errors.New("stop managed seats: missing runner")
	}
	jobs := make(chan string)
	var workers sync.WaitGroup
	var failures []error
	var failureMu sync.Mutex
	worker := func() {
		defer workers.Done()
		for name := range jobs {
			if deadlineCtx.Err() != nil {
				return
			}
			if err := stopOneSeat(deadlineCtx, name, space, runner, live); err != nil {
				failureMu.Lock()
				failures = append(failures, err)
				failureMu.Unlock()
			}
		}
	}
	count := min(8, len(names))
	workers.Add(count)
	for range count {
		go worker()
	}
feed:
	for _, name := range names {
		select {
		case jobs <- name:
		case <-deadlineCtx.Done():
			break feed
		}
	}
	close(jobs)
	workers.Wait()
	if err := deadlineCtx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	return errors.Join(failures...)
}

func stopOneSeat(ctx context.Context, name, space string, runner processRunner, live liveCheck) error {
	if live == nil {
		return fmt.Errorf("%w: no subagent check wired", errSubagentsGate)
	}
	count, err := live(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: check %s: %w", errSubagentsGate, name, err)
	}
	if count > 0 {
		return fmt.Errorf("%w: %s has %d running", errSubagentsGate, name, count)
	}
	seatCtx, seatCancel := context.WithTimeout(ctx, 30*time.Second)
	defer seatCancel()
	result, err := runner(seatCtx, []string{cotalCommand, "stop", "--name", name, spaceFlag, space}, "")
	if err == nil && result.ExitCode != 0 {
		return fmt.Errorf("cotal stop --name %s exited %d: %s", name, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return err
}
