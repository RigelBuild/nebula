package main

import (
	"context"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/samber/oops"
)

func (r *repository) forge(ctx context.Context) (*forgeClient, error) {
	configured, err := r.jj(ctx, "config", "get", "jj-vine.github.tokenCommand")
	if err != nil {
		return nil, err
	}
	var command struct {
		Argv []string `toml:"argv"`
	}
	if err := toml.Unmarshal([]byte("argv = "+configured), &command); err != nil {
		return nil, oops.Errorf("jj-vine.github.tokenCommand must be a TOML array of argv strings")
	}
	if len(command.Argv) == 0 || command.Argv[0] == "" {
		return nil, oops.Errorf("jj-vine.github.tokenCommand must name an executable")
	}
	for i, arg := range command.Argv {
		command.Argv[i] = strings.ReplaceAll(arg, "{host}", "github.com")
	}
	cmd := r.execCommand(ctx, command.Argv[0], command.Argv[1:]...)
	cmd.Dir = r.root
	cmd.Env = r.childEnv(nil)
	token, err := cmd.Output()
	if err != nil {
		return nil, oops.Errorf("jj-vine GitHub token command failed; check its actor and authorization")
	}
	value := strings.TrimSpace(string(token))
	if value == "" {
		return nil, oops.Errorf("jj-vine GitHub token command returned an empty token")
	}
	return &forgeClient{repo: r, token: value}, nil
}
