package main

import (
	"strconv"
	"time"
)

// hookTimeout reads RJC_HOOK_SSH_TIMEOUT, or defaultSSHTimeout when it is unset.
func (r sshHookRunner) hookTimeout() (time.Duration, error) {
	raw := envLookup(r.repo.env, "RJC_HOOK_SSH_TIMEOUT")
	if raw == "" {
		return defaultSSHTimeout, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return 0, refusal("hooks", "hook-ssh-config", "RJC_HOOK_SSH_TIMEOUT is not a positive duration", "set RJC_HOOK_SSH_TIMEOUT to a value like 45m")
	}
	return parsed, nil
}

// hookSlots reads RJC_HOOK_SSH_SLOTS on the client, or defaultSSHSlots when it is unset.
// The requested count is passed to remote scripts, which may further cap it per host.
func (r sshHookRunner) hookSlots() (int, error) {
	raw := envLookup(r.repo.env, "RJC_HOOK_SSH_SLOTS")
	if raw == "" {
		return defaultSSHSlots, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		return 0, refusal("hooks", "hook-ssh-config", "RJC_HOOK_SSH_SLOTS is not an integer of at least 1", "set RJC_HOOK_SSH_SLOTS to a value like 2")
	}
	return parsed, nil
}
