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

var errUsage = errors.New("usage: cd <plan|manifest|guard|publish|release-files|check-asset> [flags]")

func main() {
	// The process root context: main is the one place a root context is created.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := dispatch(ctx, args, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "cd:", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
			repo: *repo, remote: *remote, branch: *branch, attempts: *attempts, manifest: in, stderr: stderr,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, commit)
		return err
	case "release-files":
		manifest := flags.String("manifest", manifestFile, "release manifest")
		assets := flags.String("assets", "", "directory holding the assets")
		sums := flags.String("sums", "SHA256SUMS", "SHA256SUMS output path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		files, err := releaseFiles(*manifest, *assets, *sums)
		if err != nil {
			return err
		}
		for _, f := range files {
			if _, err := fmt.Fprintln(stdout, f); err != nil {
				return err
			}
		}
		return nil
	case "check-asset":
		system := flags.String("system", "", "target system, for example aarch64-darwin")
		var runtime listFlag
		flags.Var(&runtime, "runtime", "declared DT_NEEDED soname (repeatable); enables runtimeDeps")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 || *system == "" {
			return errUsage
		}
		for _, path := range flags.Args() {
			if err := checkAssetWithRuntime(path, *system, runtime); err != nil {
				return err
			}
		}
		return nil
	case "formula":
		manifest := flags.String("manifest", manifestFile, "release manifest")
		out := flags.String("out", "Formula", "directory to write Formula/<tool>.rb into")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		written, err := writeFormulas(*manifest, *out)
		if err != nil {
			return err
		}
		for _, f := range written {
			if _, err := fmt.Fprintln(stdout, f); err != nil {
				return err
			}
		}
		return nil
	case "mirror":
		prefix := flags.String("prefix", "", "repository path to split out, for example tools/jj-hooks")
		url := flags.String("url", "", "mirror clone URL, for example https://github.com/mattwilkinsonn/jj-hooks.git")
		branch := flags.String("branch", "main", "mirror branch to push")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *prefix == "" || *url == "" {
			return errUsage
		}
		split, state, err := syncMirror(ctx, mirrorOptions{
			repo: *repo, prefix: *prefix, url: *url, branch: *branch, token: os.Getenv("MIRROR_TOKEN"),
		}, stderr)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s %s %s\n", state, *url, split)
		return err
	}
	return errUsage
}

// manifestFlags registers the inputs shared by manifest and publish.
func manifestFlags(flags *flag.FlagSet) *manifestInput {
	in := &manifestInput{}
	flags.StringVar(&in.planPath, "plan", "", "plan JSON from cd plan")
	flags.StringVar(&in.tag, "tag", "", "release tag, nebula-<yyyymmdd>-<run>")
	flags.StringVar(&in.assets, "assets", "", "directory holding <pkg>-<version>-<system>.tar.zst assets")
	flags.StringVar(&in.previous, "previous-assets", "", "directory holding the previous Release's assets")
	flags.StringVar(&in.baseURL, "base-url", "", "download URL prefix; the tag and asset name are appended")
	flags.Var((*listFlag)(&in.held), "held", "package held by a red test (repeatable)")
	flags.Var((*listFlag)(&in.systems), "system", "release system (repeatable)")
	flags.Var((*listFlag)(&in.images), "image", "pushed image digest as name=sha256:<hex> (repeatable)")
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
