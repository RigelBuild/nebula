// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"os"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// profilesEnv names the environment variable that supplies the profile file
// when --profiles is not set. Nebula ships no default profile path.
const profilesEnv = "RJC_PROFILES"

// schemaCUE is the schema a caller's profile file must satisfy.
//
//go:embed schema.cue
var schemaCUE []byte

// Config is the decoded profile file: the deployment values and the repository table.
type Config struct {
	Owners          []string           `json:"owners"`
	CoAuthor        string             `json:"coAuthor"`
	CoAuthorByOwner map[string]string  `json:"coAuthorByOwner,omitempty"`
	Reviewer        string             `json:"reviewer"`
	AgentEmail      string             `json:"agentEmail"`
	MergeBotEmail   string             `json:"mergeBotEmail"`
	MergeBotRepoDir string             `json:"mergeBotRepoDir"`
	Aliases         map[string]string  `json:"aliases"`
	GateRunnerDir   string             `json:"gateRunnerDir"`
	RootChecksDir   string             `json:"rootChecksDir"`
	DevenvNixpkgs   string             `json:"devenvNixpkgs"`
	Default         *Profile           `json:"default"`
	Repos           map[string]Profile `json:"repos"`
}

// errNoProfiles reports a run that named no profile file.
var errNoProfiles = errors.New("no profile file: pass --profiles <file> or set " + profilesEnv)

// resolveProfilesPath returns the explicit flag value, else the environment
// variable. It refuses to guess a path.
func resolveProfilesPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(profilesEnv); env != "" {
		return env, nil
	}
	return "", errNoProfiles
}

// loadConfig reads the caller's profile CUE and unifies it with the embedded
// schema. The schema supplies defaults and rejects a repo that omits a required
// field, such as issuePrefix or specDirs.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The caller names the profile file.
	if err != nil {
		return Config{}, fmt.Errorf("read profiles %s: %w", path, err)
	}
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaCUE)
	if err := schema.Err(); err != nil {
		return Config{}, fmt.Errorf("compile embedded profile schema: %w", err)
	}
	value := ctx.CompileBytes(data, cue.Filename(path))
	if err := value.Err(); err != nil {
		return Config{}, fmt.Errorf("compile profiles %s: %w", path, err)
	}
	value = value.Unify(schema.LookupPath(cue.MakePath(cue.Def("Config"))))
	if err := value.Validate(cue.Concrete(true)); err != nil {
		return Config{}, fmt.Errorf("validate profiles %s: %w", path, err)
	}
	var cfg Config
	if err := value.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode profiles %s: %w", path, err)
	}
	return cfg, nil
}
