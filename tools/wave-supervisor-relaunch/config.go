// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	_ "embed"
	"fmt"
	"os"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// schemaCUE holds the #Config definition. The config file is caller input, checked against it
// before use.
//
//go:embed schema.cue
var schemaCUE []byte

// Config is the decoded --config file. Every field is a deployment value. The source carries
// none, so a host that omits one fails at load.
type Config struct {
	RootSeat         string   `json:"rootSeat"`
	CotalSpace       string   `json:"cotalSpace"`
	EndpointKind     string   `json:"endpointKind"`
	ZellijSession    string   `json:"zellijSession"`
	ZellijUnit       string   `json:"zellijUnit"`
	CotalManagerUnit string   `json:"cotalManagerUnit"`
	RelaunchUnit     string   `json:"relaunchUnit"`
	WaveSlice        string   `json:"waveSlice"`
	SeatsPath        string   `json:"seatsPath"`
	StateDir         string   `json:"stateDir"`
	HomeDir          string   `json:"homeDir"`
	SessionsDir      string   `json:"sessionsDir"`
	Hosts            []string `json:"hosts"`
	ManagerLogPath   string   `json:"managerLogPath"`
	JournalUnits     []string `json:"journalUnits"`
	RelaunchIssue    string   `json:"relaunchIssue"`
	KnownCause       string   `json:"knownCause"`
	CauseOwner       string   `json:"causeOwner"`
	SeatCLI          string   `json:"seatCLI"`
}

// loadConfig reads the --config file and checks it against #Config. Unknown fields and missing
// fields both fail, because the schema is closed and every field is required.
func loadConfig(path string) (Config, error) {
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaCUE).LookupPath(cue.ParsePath("#Config"))
	data, err := os.ReadFile(path) //nolint:gosec // The operator selects the config file with --config.
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	compiled := ctx.CompileBytes(data, cue.Filename(path))
	if err := compiled.Err(); err != nil {
		return Config{}, fmt.Errorf("compile config: %w", err)
	}
	value := compiled.Unify(schema)
	if err := value.Validate(cue.Concrete(true)); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}
	var cfg Config
	if err := value.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}
