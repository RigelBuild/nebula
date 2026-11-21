// Package layout reads the durable-state table that nemo-move copies: the home
// paths it walks, where they land in the container, and the names of the
// deployment. The tool hardcodes none of these. The operator supplies them in
// a CUE file (schema.cue), and a remote helper receives them as JSON.
package layout

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// Row kinds. Copy rows are walked and exported. Rebuild and Retire rows are
// reported only.
const (
	Copy    = "Copy"
	Rebuild = "Rebuild"
	Retire  = "Retire"
)

// StagingDir is the home directory where mac export stages Google credential
// trees. The tool owns this path, so it is never configured.
const StagingDir = ".local/state/nemo-move-staging"

//go:embed schema.cue
var schemaSource []byte

// kinds maps the CUE spelling of a row kind to its report label.
var kinds = map[string]string{"copy": Copy, "rebuild": Rebuild, "retire": Retire}

// Layout is one deployment's durable-state table.
type Layout struct {
	Rows      []Row    `json:"rows"`
	Google    Google   `json:"google"`
	Journals  Journals `json:"journals"`
	Processes []string `json:"processes"`
}

// Row is one home path. A copy row is walked and exported; other rows are
// reported only.
type Row struct {
	Path     string     `json:"path"`
	Kind     string     `json:"kind"`
	Dest     string     `json:"dest"`
	Skip     []string   `json:"skip,omitempty"`
	Discover []Discover `json:"discover,omitempty"`
}

// Discover names a directory whose unknown children are reported as copy roots.
type Discover struct {
	Dir   string   `json:"dir"`
	Known []string `json:"known,omitempty"`
}

// Google locates the staged Google credential trees.
type Google struct {
	Source   string   `json:"source"`
	Dest     string   `json:"dest"`
	Accounts []string `json:"accounts"`
}

// Journals locates the paging journals.
type Journals struct {
	Dir   string   `json:"dir"`
	Names []string `json:"names"`
}

// Load reads and validates the CUE layout at file.
func Load(file string) (Layout, error) {
	data, err := os.ReadFile(file) //nolint:gosec // The operator selects the layout file.
	if err != nil {
		return Layout{}, fmt.Errorf("read inventory: %w", err)
	}
	return Parse(data)
}

// Parse validates CUE layout data against the embedded schema. Each Google
// account adds one staging copy row.
func Parse(data []byte) (Layout, error) {
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaSource).LookupPath(cue.ParsePath("#Layout"))
	if err := schema.Err(); err != nil {
		return Layout{}, fmt.Errorf("compile inventory schema: %w", err)
	}
	value := ctx.CompileBytes(data)
	if err := value.Err(); err != nil {
		return Layout{}, fmt.Errorf("compile inventory: %w", err)
	}
	entry := value.Unify(schema)
	if err := entry.Validate(cue.Concrete(true)); err != nil {
		return Layout{}, fmt.Errorf("validate inventory: %w", err)
	}
	var decoded Layout
	if err := entry.Decode(&decoded); err != nil {
		return Layout{}, fmt.Errorf("decode inventory: %w", err)
	}
	for i := range decoded.Rows {
		decoded.Rows[i].Kind = kinds[decoded.Rows[i].Kind]
	}
	for _, account := range decoded.Google.Accounts {
		decoded.Rows = append(decoded.Rows, Row{
			Path: StagingDir + "/" + account + "/credentials",
			Kind: Copy,
			Dest: decoded.Google.Dest + "/" + account + "/credentials",
		})
	}
	if err := decoded.Validate(); err != nil {
		return Layout{}, err
	}
	return decoded, nil
}

// FromJSON decodes a layout that JSON encoded for a remote helper.
func FromJSON(data string) (Layout, error) {
	var decoded Layout
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return Layout{}, fmt.Errorf("decode layout: %w", err)
	}
	if err := decoded.Validate(); err != nil {
		return Layout{}, err
	}
	return decoded, nil
}

// JSON encodes the layout as one argument for a remote helper.
func (l Layout) JSON() (string, error) {
	data, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("encode layout: %w", err)
	}
	return string(data), nil
}

// GoogleRoot is the Google credential root relative to the transfer root.
func (l Layout) GoogleRoot() string {
	return strings.TrimPrefix(l.Google.Dest, "/")
}

// Validate checks the invariants the CUE schema cannot express.
func (l Layout) Validate() error {
	if len(l.Rows) == 0 {
		return errors.New("layout has no rows")
	}
	seen := make(map[string]bool, len(l.Rows))
	for _, row := range l.Rows {
		if row.Path == "." {
			return errors.New("row path must name a directory under the source home")
		}
		if err := checkRelative(row.Path); err != nil {
			return fmt.Errorf("row path: %w", err)
		}
		if seen[row.Path] {
			return fmt.Errorf("duplicate row %s", row.Path)
		}
		seen[row.Path] = true
		switch row.Kind {
		case Copy:
			if row.Dest != "/opt/data" && !strings.HasPrefix(row.Dest, "/opt/data/") {
				return fmt.Errorf("copy row %s: dest %q is outside /opt/data", row.Path, row.Dest)
			}
		case Rebuild, Retire:
		default:
			return fmt.Errorf("row %s: unknown kind %q", row.Path, row.Kind)
		}
		for _, name := range row.Skip {
			if err := checkRelative(name); err != nil {
				return fmt.Errorf("row %s skip: %w", row.Path, err)
			}
		}
		for _, discover := range row.Discover {
			if discover.Dir != "." {
				if err := checkRelative(discover.Dir); err != nil {
					return fmt.Errorf("row %s discover: %w", row.Path, err)
				}
			}
			for _, name := range discover.Known {
				if err := checkName(name); err != nil {
					return fmt.Errorf("row %s discover known: %w", row.Path, err)
				}
			}
		}
	}
	if !path.IsAbs(l.Google.Source) {
		return fmt.Errorf("google source %q is not absolute", l.Google.Source)
	}
	if !strings.HasPrefix(l.Google.Dest, "/opt/data/") {
		return fmt.Errorf("google dest %q is outside /opt/data", l.Google.Dest)
	}
	if len(l.Google.Accounts) == 0 {
		return errors.New("google has no accounts")
	}
	for _, account := range l.Google.Accounts {
		if err := checkName(account); err != nil {
			return fmt.Errorf("google account: %w", err)
		}
	}
	if !path.IsAbs(l.Journals.Dir) {
		return fmt.Errorf("journal dir %q is not absolute", l.Journals.Dir)
	}
	if len(l.Journals.Names) == 0 {
		return errors.New("journals have no names")
	}
	for _, name := range l.Journals.Names {
		if err := checkName(name); err != nil {
			return fmt.Errorf("journal name: %w", err)
		}
		if !strings.HasSuffix(name, ".jsonl") {
			return fmt.Errorf("journal name %q does not end in .jsonl", name)
		}
	}
	if len(l.Processes) == 0 {
		return errors.New("layout has no processes")
	}
	for _, process := range l.Processes {
		if process == "" {
			return errors.New("empty process needle")
		}
	}
	return nil
}

func checkRelative(name string) error {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || slices.Contains(strings.Split(name, "/"), "..") {
		return fmt.Errorf("%q is not a clean relative path", name)
	}
	return nil
}

func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("%q is not a plain file name", name)
	}
	return nil
}
