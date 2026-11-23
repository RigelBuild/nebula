package main

import (
	"context"
	"errors"
	"strings"
)

type workspaceSetting struct{ key, value, previous string }

func (r *repository) withVineConfig(ctx context.Context, settings []workspaceSetting, run func() error) (err error) {
	for i := range settings {
		settings[i].previous, err = r.jj(ctx, "config", "list", "--workspace", "-T", `value ++ "\n"`, settings[i].key)
		if err != nil {
			return err
		}
	}
	applied := 0
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		for i := applied - 1; i >= 0; i-- {
			setting := settings[i]
			args := []string{"config", "unset", "--workspace", setting.key}
			if setting.previous != "" {
				args = []string{"config", "set", "--workspace", setting.key, setting.previous}
			}
			_, restoreErr := r.jj(cleanup, args...)
			if restoreErr != nil && !isAlreadyUnset(restoreErr, setting.key) {
				err = errors.Join(err, restoreErr)
			}
		}
	}()
	for _, setting := range settings {
		applied++
		if _, err := r.jj(ctx, "config", "set", "--workspace", setting.key, setting.value); err != nil {
			return err
		}
	}
	return run()
}

// isAlreadyUnset reports whether a restore failed only because jj found no
// value for key. That is the goal state, so the caller treats it as restored.
func isAlreadyUnset(err error, key string) bool {
	return strings.Contains(err.Error(), `Error: "`+key+`" doesn't exist`)
}
