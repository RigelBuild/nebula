package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

func stopManagedSeats(ctx context.Context, names []string, space string, runner processRunner) error {
	if len(names) == 0 {
		return nil
	}
	if runner == nil {
		return errors.New("stop managed seats: missing runner")
	}
	return stopManagedSeatsWithDeadline(ctx, names, space, runner, time.Duration(seatStopDeadlineMS)*time.Millisecond, context.WithTimeout)
}

func stopManagedSeatsWithDeadline(ctx context.Context, names []string, space string, runner processRunner, deadline time.Duration, withDeadline func(context.Context, time.Duration) (context.Context, context.CancelFunc)) error {
	if len(names) == 0 {
		return nil
	}
	if runner == nil {
		return errors.New("stop managed seats: missing runner")
	}
	deadlineCtx, cancel := withDeadline(ctx, deadline)
	defer cancel()
	return stopManagedSeatsUntil(deadlineCtx, names, space, runner)
}

func stopManagedSeatsUntil(deadlineCtx context.Context, names []string, space string, runner processRunner) error {
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
			if err := stopOneSeat(deadlineCtx, name, space, runner); err != nil {
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

func stopOneSeat(ctx context.Context, name, space string, runner processRunner) error {
	seatCtx, seatCancel := context.WithTimeout(ctx, 30*time.Second)
	result, err := runner(seatCtx, []string{cotalCommand, "stop", "--name", name, spaceFlag, space}, "")
	seatCancel()
	if err == nil && result.ExitCode != 0 {
		return fmt.Errorf("cotal stop --name %s exited %d: %s", name, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return err
}
