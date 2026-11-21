package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// schemaCUE holds the #Host definition. The inventory itself is caller input.
//
//go:embed schema.cue
var schemaCUE []byte

type host struct {
	name, target, platform, flake, user, identityFile, gamingFlag, gamingProbeHost string
	windowsCheckout, windowsSetup                                                  string
	port                                                                           int
	rebootable, explicitOnly, localCheckout                                        bool
}

type hostConfig struct {
	Name            string `json:"name"`
	Target          string `json:"target"`
	Platform        string `json:"platform"`
	Flake           string `json:"flake"`
	Rebootable      bool   `json:"rebootable"`
	User            string `json:"user"`
	Port            int    `json:"port"`
	IdentityFile    string `json:"identityFile"`
	GamingFlag      string `json:"gamingFlag"`
	GamingProbeHost string `json:"gamingProbeHost"`
	ExplicitOnly    bool   `json:"explicitOnly"`
	LocalCheckout   bool   `json:"localCheckout"`
	WindowsCheckout string `json:"windowsCheckout"`
	WindowsSetup    string `json:"windowsSetup"`
}

// loadHosts reads the CUE inventory at path. The inventory must be supplied by the caller.
func loadHosts(path string) ([]host, error) {
	if path == "" {
		return nil, fmt.Errorf("no hosts inventory; pass --hosts FILE")
	}
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaCUE).LookupPath(cue.ParsePath("#Host"))
	data, err := os.ReadFile(path) //nolint:gosec // The operator explicitly selects the inventory file.
	if err != nil {
		return nil, fmt.Errorf("read hosts: %w", err)
	}
	value := ctx.CompileBytes(data)
	if err := value.Err(); err != nil {
		return nil, fmt.Errorf("compile hosts: %w", err)
	}
	iterator, err := value.LookupPath(cue.ParsePath("hosts")).List()
	if err != nil {
		return nil, fmt.Errorf("hosts: %w", err)
	}
	result := []host{}
	seen := make(map[string]struct{})
	for iterator.Next() {
		entry := iterator.Value().Unify(schema)
		if err := entry.Validate(cue.Concrete(true)); err != nil {
			return nil, fmt.Errorf("validate host: %w", err)
		}
		var decoded hostConfig
		if err := entry.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("decode host: %w", err)
		}
		if _, exists := seen[decoded.Name]; exists {
			return nil, fmt.Errorf("duplicate host name %q", decoded.Name)
		}
		seen[decoded.Name] = struct{}{}
		if strings.HasPrefix(decoded.IdentityFile, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve identity file: %w", err)
			}
			decoded.IdentityFile = filepath.Join(home, decoded.IdentityFile[2:])
		}
		result = append(result, host{
			name: decoded.Name, target: decoded.Target, platform: decoded.Platform,
			flake: decoded.Flake, rebootable: decoded.Rebootable, user: decoded.User,
			port: decoded.Port, identityFile: decoded.IdentityFile, gamingFlag: decoded.GamingFlag,
			gamingProbeHost: decoded.GamingProbeHost, explicitOnly: decoded.ExplicitOnly,
			localCheckout: decoded.LocalCheckout, windowsCheckout: decoded.WindowsCheckout,
			windowsSetup: decoded.WindowsSetup,
		})
	}
	if err := checkGamingProbeHosts(result); err != nil {
		return nil, err
	}
	return result, nil
}

// checkGamingProbeHosts rejects a gamingProbeHost that is unknown, names the host itself, or names a windows host.
func checkGamingProbeHosts(hosts []host) error {
	byName := make(map[string]host, len(hosts))
	for _, h := range hosts {
		byName[h.name] = h
	}
	for _, h := range hosts {
		if h.gamingProbeHost == "" {
			continue
		}
		probe, ok := byName[h.gamingProbeHost]
		switch {
		case !ok:
			return fmt.Errorf("host %s: gamingProbeHost %q is not a host in this inventory", h.name, h.gamingProbeHost)
		case h.gamingProbeHost == h.name:
			return fmt.Errorf("host %s: gamingProbeHost names the host itself", h.name)
		case probe.platform == platformWindows:
			return fmt.Errorf("host %s: gamingProbeHost %q is a windows host; probe its WSL host instead", h.name, h.gamingProbeHost)
		}
	}
	return nil
}
