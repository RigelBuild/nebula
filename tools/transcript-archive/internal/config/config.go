package config

import (
	_ "embed"
	"fmt"
	"os"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// schemaSource is the CUE schema every archive config is validated against. It ships inside the binary.
//
//go:embed schema.cue
var schemaSource []byte

// Config holds host walk sets loaded from CUE.
type Config struct {
	hosts map[string]Walk
}

// Load reads and validates host walk configuration against the embedded schema.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path is the operator-supplied config file.
	if err != nil {
		return Config{}, fmt.Errorf("read archive config: %w", err)
	}
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaSource)
	if err := schema.Err(); err != nil {
		return Config{}, fmt.Errorf("compile archive schema: %w", err)
	}
	schemaHosts := schema.LookupPath(cue.ParsePath("hosts"))
	dataValue := ctx.CompileBytes(data)
	if err := dataValue.Err(); err != nil {
		return Config{}, fmt.Errorf("compile archive config: %w", err)
	}
	value := schemaHosts.Unify(dataValue.LookupPath(cue.ParsePath("hosts")))
	if err := value.Validate(cue.Concrete(true)); err != nil {
		return Config{}, fmt.Errorf("validate archive config: %w", err)
	}
	var decoded struct {
		Hosts map[string]struct {
			State     string      `json:"state"`
			Sources   []Source    `json:"sources"`
			Watch     []string    `json:"watch"`
			Exclude   []Exclusion `json:"exclude"`
			FinalOnly []string    `json:"finalOnly"`
		} `json:"hosts"`
	}
	if err := value.Decode(&decoded.Hosts); err != nil {
		return Config{}, fmt.Errorf("decode archive config: %w", err)
	}
	hosts := make(map[string]Walk, len(decoded.Hosts))
	for name, host := range decoded.Hosts {
		hosts[name] = Walk{
			State:     host.State,
			Sources:   host.Sources,
			Watch:     host.Watch,
			Exclude:   host.Exclude,
			FinalOnly: host.FinalOnly,
		}
	}
	return Config{hosts: hosts}, nil
}

// Walk returns the configured source roots and watch set for a host.
func (c Config) Walk(host string) (Walk, error) {
	walk, ok := c.hosts[host]
	if !ok {
		return Walk{}, fmt.Errorf("unknown archive host %q", host)
	}
	return walk, nil
}
