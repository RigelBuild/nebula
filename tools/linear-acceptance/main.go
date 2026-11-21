// linear-acceptance runs the real linear CLI, refusing an issue create with no acceptance criteria.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

const escapeFlag = "--no-acceptance-criteria"

var (
	headingPattern = regexp.MustCompile(`^## Acceptance criteria\s*$`)
	itemPattern    = regexp.MustCompile(`^\s*- \[ \] \S`)
	fenceOpen      = regexp.MustCompile("^\\s*(`{3,}|~{3,})")

	errNoHeading       = errors.New(`description has no "## Acceptance criteria" heading`)
	errNoItem          = errors.New(`"## Acceptance criteria" has no "- [ ]" item`)
	errTwoDescriptions = errors.New("linear-cli refuses two description options (-d/--description and --description-file)")
)

type descriptionSource struct {
	file  bool
	value string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "linear-acceptance: usage: linear-acceptance <real-linear> <args...>")
		os.Exit(2)
	}
	realLinear := os.Args[1]
	args, err := rewrite(os.Args[2:], os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "linear-acceptance: refused: %v\n", err)
		os.Exit(2)
	}
	argv := append([]string{realLinear}, args...)
	if err := syscall.Exec(realLinear, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "linear-acceptance: exec %s: %v\n", realLinear, err)
		os.Exit(1)
	}
}

// rewrite returns the arguments for linear. Only `issue create` (or `i create`) is checked; help and
// other commands pass through. A waiver writes one line to notice.
func rewrite(args []string, notice io.Writer) ([]string, error) {
	next := createStart(args)
	if next < 0 {
		return args, nil
	}
	kept := slices.Clone(args[:next])
	var source *descriptionSource
	var waiver string
	waived := false
	inline, file := false, false
	for i := next; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// linear-cli ignores what follows "--", so nothing after it is read or checked.
			kept = append(kept, args[i:]...)
			break
		}
		if arg == "-h" || arg == "--help" {
			// Help in flag position: the CLI prints help and creates nothing.
			return args, nil
		}
		if isShortCluster(arg) {
			// cliffy splits -tal into -t -a -l. Splice the letters in so the walk below handles each as a flag.
			letters := make([]string, 0, len(arg)-1)
			for _, c := range arg[1:] {
				letters = append(letters, "-"+string(c))
			}
			args = slices.Replace(args, i, i+1, letters...)
			i--
			continue
		}
		flag, value, hasValue := splitFlag(arg)
		if !takesValue(flag) {
			kept = append(kept, arg)
			continue
		}
		tokens := []string{arg}
		if !hasValue && i+1 < len(args) {
			// The CLI takes the next token as the value, even when it starts with "-".
			i++
			value = args[i]
			tokens = append(tokens, value)
		} else if !hasValue && flag != escapeFlag {
			return nil, fmt.Errorf("%s needs a value", flag)
		}
		switch flag {
		case escapeFlag:
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s needs a non-empty reason", escapeFlag)
			}
			waiver, waived = value, true
			continue
		case "--description-file":
			file = true
			source = &descriptionSource{file: true, value: value}
		case "-d", "--description":
			inline = true
			source = &descriptionSource{value: value}
		}
		kept = append(kept, tokens...)
	}
	if inline && file {
		return nil, errTwoDescriptions
	}
	if waived {
		fmt.Fprintf(notice, "linear-acceptance: acceptance criteria waived: %q\n", waiver)
		return kept, nil
	}
	text, err := descriptionText(source)
	if err != nil {
		return nil, err
	}
	if err := checkAcceptance(text); err != nil {
		return nil, fmt.Errorf("%w; add it or pass %s <reason>", err, escapeFlag)
	}
	return kept, nil
}

// createStart returns the index just past `issue create` or `i create`, or -1 when args do not name
// that command. Root and issue-level --workspace options may come before the names.
func createStart(args []string) int {
	i := skipWorkspace(args, 0)
	if i >= len(args) || (args[i] != "issue" && args[i] != "i") {
		return -1
	}
	i = skipWorkspace(args, i+1)
	if i >= len(args) || args[i] != "create" {
		return -1
	}
	return i + 1
}

// skipWorkspace returns the index of the first token at or after i that is not a --workspace option.
// "--workspace <slug>" takes two tokens. "--workspace=<slug>" takes one, and an empty "--workspace=" takes
// the next token as its slug, as linear-cli does.
func skipWorkspace(args []string, i int) int {
	for i < len(args) {
		switch {
		case args[i] == "--workspace" && i+1 < len(args):
			i += 2
		case args[i] == "--workspace=" && i+1 < len(args):
			i += 2
		case strings.HasPrefix(args[i], "--workspace="):
			i++
		default:
			return i
		}
	}
	return i
}

// isShortCluster reports whether arg is a short-flag cluster such as -tal: one dash, two or more ASCII
// letters, and no "=". "-t=" and "-d=x" are single flags, not clusters.
func isShortCluster(arg string) bool {
	if len(arg) < 3 || arg[0] != '-' || arg[1] == '-' || strings.Contains(arg, "=") {
		return false
	}
	for _, c := range arg[1:] {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// checkAcceptance requires a "- [ ]" item inside the acceptance criteria section, before the next "## " heading.
// Lines inside a fenced code block are not read. Checked items ("- [x]" or "- [X]") do not count.
func checkAcceptance(text string) error {
	seen, inSection := false, false
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		if fence != "" {
			if closesFence(line, fence) {
				fence = ""
			}
			continue
		}
		if m := fenceOpen.FindStringSubmatch(line); m != nil {
			fence = m[1]
			continue
		}
		switch {
		case headingPattern.MatchString(line):
			seen, inSection = true, true
		case strings.HasPrefix(line, "## "):
			inSection = false
		case inSection && itemPattern.MatchString(line):
			return nil
		}
	}
	if !seen {
		return errNoHeading
	}
	return errNoItem
}

// closesFence reports whether line closes a fence opened with marker: the same character, at least as long, and nothing else.
func closesFence(line, marker string) bool {
	t := strings.TrimSpace(line)
	return len(t) >= len(marker) && strings.Trim(t, marker[:1]) == ""
}

// descriptionText returns the inline description, or the contents of the description file.
func descriptionText(source *descriptionSource) (string, error) {
	switch {
	case source == nil:
		return "", nil
	case source.file:
		return readDescriptionFile(source.value)
	default:
		return source.value, nil
	}
}

// readDescriptionFile reads the file a caller names with --description-file.
func readDescriptionFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read description file %s: %w", path, err)
	}
	return string(data), nil
}

// splitFlag splits "--flag=value" into its parts; a non-flag argument is returned whole. An empty value after "="
// is no value, as in linear-cli (cliffy: slice(eq+1) || undefined), so the next token is taken as the value.
func splitFlag(arg string) (flag, value string, hasValue bool) {
	if !strings.HasPrefix(arg, "-") {
		return arg, "", false
	}
	flag, value, hasValue = strings.Cut(arg, "=")
	return flag, value, hasValue && value != ""
}

// takesValue reports whether a flag takes its value from the next token. The list is the value options of
// `linear issue create --help` (linear-cli 2.6.0), --workspace at every level, and this tool's escape flag.
func takesValue(flag string) bool {
	switch flag {
	case "-a", "--assignee", "--due-date", "--parent", "-p", "--priority", "--estimate",
		"-d", "--description", "--description-file", "-l", "--label", "--team", "--project",
		"-s", "--state", "--milestone", "--cycle", "-t", "--title", "--workspace", escapeFlag:
		return true
	}
	return false
}
