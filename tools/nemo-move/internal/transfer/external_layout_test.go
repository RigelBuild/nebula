package transfer_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
)

// exampleLayout loads the made-up deployment in testdata/inventory.cue, the
// same file operators pass with --inventory. Load expands the Google accounts
// into their staging rows, as production does.
func exampleLayout(t *testing.T) layout.Layout {
	t.Helper()
	lay, err := layout.Load(filepath.Join("..", "..", "testdata", "inventory.cue"))
	if err != nil {
		t.Fatal(err)
	}
	return lay
}

// exampleLayoutJSON returns the made-up deployment in the JSON form the
// mac-side and verify commands receive, for callers without a testing.T.
func exampleLayoutJSON() (string, error) {
	lay, err := layout.Load(filepath.Join("..", "..", "testdata", "inventory.cue"))
	if err != nil {
		return "", err
	}
	return lay.JSON()
}

// layoutFromCommand decodes the layout JSON that a mac-side command carries as
// its last single-quoted argument. The argument follows the subcommand word.
func layoutFromCommand(cmd string) (layout.Layout, error) {
	_, arg, ok := strings.Cut(cmd, " mac-side ")
	if !ok {
		return layout.Layout{}, errors.New("command has no mac-side subcommand")
	}
	_, quoted, ok := strings.Cut(arg, " ")
	if !ok || !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
		return layout.Layout{}, errors.New("command has no layout argument")
	}
	return layout.FromJSON(strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'"))
}
