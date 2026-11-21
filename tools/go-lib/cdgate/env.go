package cdgate

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

// ParsePositiveInt parses CI_PIPELINE_NUMBER and similar: an integer of at least 1.
func ParsePositiveInt(value, name string) (int64, error) {
	return woodpecker.ParseBoundedInt(value, name, 1)
}

// ParseNonNegativeInt parses CI_PIPELINE_PARENT: zero is allowed, meaning no parent.
func ParseNonNegativeInt(value, name string) (int64, error) {
	return woodpecker.ParseBoundedInt(value, name, 0)
}

// OwnFromEnv reads the own pipeline's identity from the CI environment. The
// target is taken from the step, and the commit must be present.
func OwnFromEnv(env func(string) string, target string) (Own, error) {
	number, err := ParsePositiveInt(strings.TrimSpace(env("CI_PIPELINE_NUMBER")), "CI_PIPELINE_NUMBER")
	if err != nil {
		return Own{}, err
	}
	parent, err := ParseNonNegativeInt(strings.TrimSpace(env("CI_PIPELINE_PARENT")), "CI_PIPELINE_PARENT")
	if err != nil {
		return Own{}, err
	}
	commit := strings.TrimSpace(env("CI_COMMIT_SHA"))
	if commit == "" {
		return Own{}, errors.New("missing CI_COMMIT_SHA")
	}
	return Own{Number: number, Commit: commit, Parent: parent, Target: target}, nil
}

// PipelineAPIFromEnv resolves the Woodpecker API context from the environment,
// including the repo ID, and returns the pipeline reader for it. inClusterURL
// is the Woodpecker API address reachable from inside the cluster; the caller
// must supply it, because no default is safe for every deployment.
func PipelineAPIFromEnv(ctx context.Context, env func(string) string, inClusterURL string, doer *http.Client) (PipelineAPI, error) {
	wp, err := woodpecker.ContextFromEnvWithRepoID(ctx, env, inClusterURL, doer)
	if err != nil {
		return PipelineAPI{}, err
	}
	client := &woodpecker.Client{BaseURL: wp.BaseURL, Token: wp.Token, HTTP: doer}
	return PipelineAPI{Client: client, RepoID: wp.RepoID}, nil
}
