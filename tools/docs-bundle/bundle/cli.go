// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
)

const usage = "Usage: docs-bundle check [--root .] [--config required|default]\n"

var collisionPattern = regexp.MustCompile(`^(\d+) page\(s\) (.+)$`)

// Main runs the check command with injected arguments and output streams.
func Main(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseArgs(args)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "docs-bundle: %s\n%s", err, usage); writeErr != nil {
			return 2
		}
		return 2
	}
	return check(parsed.root, parsed.config, stdout, stderr)
}

type cliArgs struct {
	root   string
	config ConfigMode
}

func parseArgs(args []string) (cliArgs, error) {
	if len(args) == 0 || args[0] != "check" {
		return cliArgs{}, fmt.Errorf("expected the check subcommand")
	}
	var root *string
	var config *ConfigMode
	for index := 1; index < len(args); index++ {
		argument := args[index]
		if argument != "--root" && argument != "--config" {
			return cliArgs{}, fmt.Errorf("unknown argument %q", argument)
		}
		if index+1 >= len(args) || args[index+1] == "" || len(args[index+1]) >= 2 && args[index+1][:2] == "--" {
			return cliArgs{}, fmt.Errorf("%s requires a value", argument)
		}
		value := args[index+1]
		if argument == "--root" {
			if root != nil {
				return cliArgs{}, fmt.Errorf("--root may appear once")
			}
			root = new(value)
		} else {
			if config != nil {
				return cliArgs{}, fmt.Errorf("--config may appear once")
			}
			mode := ConfigMode(value)
			if mode != ConfigRequired && mode != ConfigDefault {
				return cliArgs{}, fmt.Errorf("--config must be required or default")
			}
			config = &mode
		}
		index++
	}
	rootValue := "."
	if root != nil {
		rootValue = *root
	}
	rootPath, err := filepath.Abs(rootValue)
	if err != nil {
		return cliArgs{}, fmt.Errorf("resolving root: %w", err)
	}
	configValue := ConfigRequired
	if config != nil {
		configValue = *config
	}
	return cliArgs{root: rootPath, config: configValue}, nil
}

func check(root string, mode ConfigMode, stdout, stderr io.Writer) int {
	config, err := ReadPublishConfig(root, mode)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "docs-bundle: configuration error: %s\n", err); writeErr != nil {
			return 2
		}
		return 2
	}
	source := FetchedSource{
		BundleSource: BundleSource{Repo: "local/worktree", Mount: "", Config: mode},
		Root:         root,
	}
	pages, err := CollectPages(source, config)
	if err != nil {
		if writePageError(stderr, err.Error()) != nil {
			return 2
		}
		return 1
	}
	if err := AssertNoRouteCollisions(pages); err != nil {
		if writePageError(stderr, err.Error()) != nil {
			return 2
		}
		return 1
	}
	broken, err := FindBrokenLinks(root, source.Repo, pages)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "docs-bundle: page read error: %s\n", err); writeErr != nil {
			return 2
		}
		return 2
	}
	if len(broken) > 0 {
		if _, writeErr := fmt.Fprintf(stderr, "docs-bundle: %d broken link(s)\n", len(broken)); writeErr != nil {
			return 1
		}
		for _, link := range broken {
			if _, writeErr := fmt.Fprintf(stderr, "  %s -> %s\n", link.SourcePath, link.Target); writeErr != nil {
				return 1
			}
		}
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "docs-bundle: checked %d page(s); no broken links\n", len(pages)); err != nil {
		return 2
	}
	return 0
}

func writePageError(stderr io.Writer, message string) error {
	match := collisionPattern.FindStringSubmatch(message)
	if len(match) >= 3 {
		_, err := fmt.Fprintf(stderr, "docs-bundle: %s page(s) %s\n", match[1], match[2])
		return err
	}
	_, err := fmt.Fprintf(stderr, "docs-bundle: 1 page(s) %s\n", message)
	return err
}
