// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestDecodeConfig(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    config
		wantErr string
	}{
		{name: "valid", body: validConfig, want: config{Repo: "/repo", Flake: "example/nix", Hosts: []string{"example-host", "other-host"}}},
		{name: "relative repo", body: "repo: \"repos/example\"\nflake: \"example/nix\"\nhosts: [\"example-host\"]\n", want: config{Repo: "repos/example", Flake: "example/nix", Hosts: []string{"example-host"}}},
		{name: "missing repo", body: "flake: \"example/nix\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "empty repo", body: "repo: \"\"\nflake: \"example/nix\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "absolute flake", body: "repo: \"/repo\"\nflake: \"/abs/nix\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "parent segment first", body: "repo: \"/repo\"\nflake: \"../nix\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "parent segment middle", body: "repo: \"/repo\"\nflake: \"example/../nix\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "parent segment last", body: "repo: \"/repo\"\nflake: \"example/..\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "bare parent", body: "repo: \"/repo\"\nflake: \"..\"\nhosts: [\"example-host\"]\n", wantErr: "validate config"},
		{name: "dots inside a name", body: "repo: \"/repo\"\nflake: \"example/a..b\"\nhosts: [\"example-host\"]\n", want: config{Repo: "/repo", Flake: "example/a..b", Hosts: []string{"example-host"}}},
		{name: "missing hosts", body: "repo: \"/repo\"\nflake: \"example/nix\"\n", wantErr: "validate config"},
		{name: "no hosts", body: "repo: \"/repo\"\nflake: \"example/nix\"\nhosts: []\n", wantErr: "validate config"},
		{name: "empty host name", body: "repo: \"/repo\"\nflake: \"example/nix\"\nhosts: [\"\"]\n", wantErr: "validate config"},
		{name: "unknown field", body: validConfig + "extra: \"x\"\n", wantErr: "validate config"},
		{name: "import", body: "import \"strings\"\n" + validConfig, wantErr: "imports are not permitted"},
		{name: "syntax", body: "repo: \n", wantErr: "parse config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeConfig([]byte(tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("config %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigRepo(t *testing.T) {
	if got := configRepo("/fixture", "repos/example"); got != "/fixture/repos/example" {
		t.Errorf("relative repo resolved to %q", got)
	}
	if got := configRepo("/fixture", "/srv/example"); got != "/srv/example" {
		t.Errorf("absolute repo resolved to %q", got)
	}
}
