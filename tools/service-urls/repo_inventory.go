// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/githubapptoken"
	"github.com/RigelBuild/nebula/tools/service-urls/config"
)

const repoInventoryURL = "https://api.github.com/orgs/RigelBuild/repos?per_page=100"

type orgRepo struct {
	name     string
	archived bool
}

type managedRepos struct {
	Active    []string `json:"active"`
	Unmanaged []string `json:"unmanaged"`
}

type repoInventoryOptions struct {
	reposText string
	mint      func(context.Context) (githubapptoken.Token, error)
	client    *http.Client
	stdout    io.Writer
	stderr    io.Writer
}

func runRepoInventory(ctx context.Context, root string, cfg config.Config, getenv func(string) string, stdout, stderr io.Writer) int {
	privateKey := getenv("REPO_INVENTORY_APP_PRIVATE_KEY")
	clientID := getenv("REPO_INVENTORY_APP_CLIENT_ID")
	repo := getenv("REPO")
	missing := make([]string, 0, 3)
	if privateKey == "" {
		missing = append(missing, "REPO_INVENTORY_APP_PRIVATE_KEY")
	}
	if clientID == "" {
		missing = append(missing, "REPO_INVENTORY_APP_CLIENT_ID")
	}
	if repo == "" {
		missing = append(missing, "REPO")
	}
	if len(missing) > 0 {
		return writeFailure(stderr, "repo-inventory: missing required env: "+strings.Join(missing, ", "))
	}
	data, err := os.ReadFile(filepath.Join(root, cfg.ManagedReposPath)) //nolint:gosec // Inventory path comes from the deployment config.
	if err != nil {
		return writeFailure(stderr, fmt.Sprintf("repo-inventory: cannot read %s: %v", cfg.ManagedReposPath, err))
	}
	return runRepoInventoryWithOptions(ctx, repoInventoryOptions{
		reposText: string(data),
		mint: func(ctx context.Context) (githubapptoken.Token, error) {
			return githubapptoken.Mint(ctx, clientID, repo, []byte(privateKey))
		},
		client: http.DefaultClient,
		stdout: stdout,
		stderr: stderr,
	})
}

func runRepoInventoryWithOptions(ctx context.Context, opts repoInventoryOptions) int {
	repos, err := parseManagedRepos(opts.reposText)
	if err != nil {
		return writeFailure(opts.stderr, "repo-inventory: cannot parse inventory: "+err.Error())
	}
	token, err := opts.mint(ctx)
	if err != nil {
		return writeFailure(opts.stderr, "repo-inventory: cannot mint installation token")
	}
	client := opts.client
	if client == nil {
		client = http.DefaultClient
	}
	client = withoutRedirects(client)
	org, err := readOrgRepos(ctx, client, string(token))
	if err != nil {
		return writeFailure(opts.stderr, "repo-inventory: "+err.Error())
	}
	gaps := inventoryGaps(org, repos)
	if len(gaps) > 0 {
		for _, gap := range gaps {
			if !writeLine(opts.stderr, "repo-inventory: "+gap) {
				return 2
			}
		}
		return 1
	}
	active := 0
	for _, repo := range org {
		if !repo.archived {
			active++
		}
	}
	if !writeLine(opts.stdout, fmt.Sprintf("repo-inventory: %d repositories match", active)) {
		return 2
	}
	return 0
}

func parseManagedRepos(text string) (managedRepos, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return managedRepos{}, errors.New("inventory must contain active and unmanaged string lists")
	}
	if value == nil {
		return managedRepos{}, errors.New("inventory must contain active and unmanaged string lists")
	}
	var repos managedRepos
	active, hasActive := value["active"]
	unmanaged, hasUnmanaged := value["unmanaged"]
	if !hasActive || !hasUnmanaged || json.Unmarshal(active, &repos.Active) != nil || json.Unmarshal(unmanaged, &repos.Unmanaged) != nil || repos.Active == nil || repos.Unmanaged == nil {
		return managedRepos{}, errors.New("inventory must contain active and unmanaged string lists")
	}
	return repos, nil
}

func parseOrgRepos(data []byte) ([]orgRepo, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || values == nil {
		return nil, errors.New("response must be an array")
	}
	repos := make([]orgRepo, 0, len(values))
	for index, value := range values {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("response repo at index %d must have name and archived fields", index)
		}
		var name string
		var archived bool
		if json.Unmarshal(fields["name"], &name) != nil || json.Unmarshal(fields["archived"], &archived) != nil || name == "" {
			return nil, fmt.Errorf("response repo at index %d must have name and archived fields", index)
		}
		repos = append(repos, orgRepo{name: name, archived: archived})
	}
	return repos, nil
}

func inventoryGaps(org []orgRepo, repos managedRepos) []string {
	declared := make(map[string]struct{}, len(repos.Active)+len(repos.Unmanaged))
	for _, name := range append(slices.Clone(repos.Active), repos.Unmanaged...) {
		declared[strings.ToLower(name)] = struct{}{}
	}
	live := make(map[string]struct{}, len(org))
	gaps := make([]string, 0)
	for _, repo := range org {
		if repo.archived {
			continue
		}
		lower := strings.ToLower(repo.name)
		live[lower] = struct{}{}
		if _, exists := declared[lower]; !exists {
			gaps = append(gaps, "undeclared repo: "+repo.name)
		}
	}
	for _, name := range append(slices.Clone(repos.Active), repos.Unmanaged...) {
		if _, exists := live[strings.ToLower(name)]; !exists {
			gaps = append(gaps, "missing repo: "+name)
		}
	}
	sort.Strings(gaps)
	return gaps
}
