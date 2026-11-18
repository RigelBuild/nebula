// SPDX-License-Identifier: MIT OR Apache-2.0

// Package main implements the cd executable, which plans, records and publishes
// nebula Releases for .github/workflows/cd.yaml.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

var errUsage = errors.New("usage: cd <plan|manifest|guard|publish|check-asset> [flags]")

func main() {
	// The process root context: main is the one place a root context is created.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := dispatch(ctx, args, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "cd:", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	repo := flags.String("repo", ".", "repository directory")
	switch args[0] {
	case "plan":
		out := flags.String("out", "", "plan JSON output path (default stdout)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		p, err := makePlan(ctx, *repo)
		if err != nil {
			return err
		}
		return writeJSON(*out, stdout, p)
	case "manifest":
		in := manifestFlags(flags)
		out := flags.String("out", manifestFile, "manifest output path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		data, err := in.build()
		if err != nil {
			return err
		}
		return os.WriteFile(*out, data, 0o644)
	case "guard":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errUsage
		}
		return guard(ctx, *repo, flags.Arg(0))
	case "publish":
		in := manifestFlags(flags)
		remote := flags.String("remote", "origin", "remote to push to")
		branch := flags.String("branch", "main", "branch to commit to")
		attempts := flags.Int("attempts", 5, "push attempts")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		commit, err := publish(ctx, publishOptions{
			repo: *repo, remote: *remote, branch: *branch, attempts: *attempts, manifest: in,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, commit)
		return err
	case "check-asset":
		system := flags.String("system", "", "target system, for example aarch64-darwin")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 || *system == "" {
			return errUsage
		}
		for _, path := range flags.Args() {
			if err := checkAsset(path, *system); err != nil {
				return err
			}
		}
		return nil
	}
	return errUsage
}

// manifestFlags registers the inputs shared by manifest and publish.
func manifestFlags(flags *flag.FlagSet) *manifestInput {
	in := &manifestInput{}
	flags.StringVar(&in.planPath, "plan", "", "plan JSON from cd plan")
	flags.StringVar(&in.tag, "tag", "", "release tag, nebula-<yyyymmdd>-<run>")
	flags.StringVar(&in.assets, "assets", "", "directory holding <pkg>-<version>-<system>.tar.zst assets")
	flags.StringVar(&in.baseURL, "base-url", "", "download URL prefix; the tag and asset name are appended")
	flags.Var((*listFlag)(&in.held), "held", "package held by a red test (repeatable)")
	flags.Var((*listFlag)(&in.systems), "system", "release system (repeatable)")
	return in
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, item := range strings.Fields(strings.ReplaceAll(v, ",", " ")) {
		*l = append(*l, item)
	}
	return nil
}
