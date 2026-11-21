package inventory

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/config"
)

func TestWalkOrphanSymlinkAndOutside(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "sessions", "orphan"))
	mustWrite(t, filepath.Join(root, "sessions", "orphan", "file.log"), "not a root session")
	target := filepath.Join(t.TempDir(), "outside.jsonl")
	mustWrite(t, target, "outside")
	mustWrite(t, filepath.Join(root, "custom-session-files", "marker"), target+"\n")
	link := filepath.Join(root, "sessions", "orphan", "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cfg := config.Walk{Root: root, Sources: []config.Source{
		{Name: "sessions", Path: "sessions"},
		{Name: "custom-session-files", Path: "custom-session-files"},
	}}
	got, _, err := Walk(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(got.Lines, filepath.Join(root, "sessions/orphan/file.log"), "tree/unassigned-sessions") {
		t.Fatalf("orphan record missing: %#v", got.Lines)
	}
	if !hasType(got.Lines, filepath.Join(root, "sessions/orphan/link"), "symlink") {
		t.Fatalf("symlink not recorded: %#v", got.Lines)
	}
	if got.OutsideTargets != 1 {
		t.Fatalf("outside targets = %d, want 1", got.OutsideTargets)
	}
}

func TestWalkRootUnderSessions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions", "seat")
	mustWrite(t, filepath.Join(root, "data", "file.log"), "data")
	mustWrite(t, filepath.Join(root, "sessions", "project", "one.jsonl"), "{\"type\":\"session\",\"id\":\"one\"}\n")
	got, _, err := Walk(config.Walk{Root: root, Sources: []config.Source{{Name: "data", Path: "data"}, {Name: "sessions", Path: "sessions"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(got.Lines, filepath.Join(root, "data", "file.log"), "tree/data") || !hasLine(got.Lines, filepath.Join(root, "sessions", "project", "one.jsonl"), "omp-session/one") {
		t.Fatalf("wrong records: %#v", got.Lines)
	}
}

func TestWalkDuplicateSession(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		file := filepath.Join(root, "sessions", name, name+".jsonl")
		mustMkdir(t, filepath.Dir(file))
		mustWrite(t, file, "{\"type\":\"session\",\"id\":\"same-id\"}\n")
	}
	got, _, err := Walk(config.Walk{Root: root, Sources: []config.Source{{Name: "sessions", Path: "sessions"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 1 {
		t.Fatalf("assignment errors = %d, want 1: %#v", len(got.Errors), got.Errors)
	}
}

func TestWalkUnknownWatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".omp", "new-store"))
	mustWrite(t, filepath.Join(root, ".omp", "new-store", "data"), "data")
	got, _, err := Walk(config.Walk{
		Root: root, Sources: []config.Source{{Name: "known", Path: "known"}}, Watch: []string{".omp"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 1 || got.Errors[0].Reason != "unknown watched file" {
		t.Fatalf("errors = %#v, want unknown watched file", got.Errors)
	}
}

func TestWalkExcluded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "data", "config.yml"), "secret-materialized-config")
	got, _, err := Walk(config.Walk{
		Root: root, Sources: []config.Source{{Name: "data", Path: "data"}},
		Exclude: []config.Exclusion{{Path: "data/config.yml"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Count != 1 || got.Excluded[0].Bytes != int64(len("secret-materialized-config")) {
		t.Fatalf("excluded summary = %#v", got.Excluded)
	}
	encoded, err := json.Marshal(got.Excluded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "config.yml") || strings.Contains(string(encoded), "secret-materialized-config") {
		t.Fatalf("excluded record leaked path or contents: %s", encoded)
	}
}

func TestWalkMissingReferences(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "stem", "stem.jsonl")
	mustWrite(t, path, "{\"type\":\"session\",\"id\":\"one\",\"blob\":\"blob:sha256:"+strings.Repeat("a", 64)+"\",\"artifact\":\"artifact://7\"}\n")
	got, _, err := Walk(config.Walk{Root: root, Sources: []config.Source{{Name: "sessions", Path: "sessions"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reasons := make(map[string]bool)
	for _, item := range got.Dangling {
		reasons[item.Reason] = true
	}
	if !reasons["missing"] || !reasons["artifact-missing"] {
		t.Fatalf("dangling reasons: %#v", got.Dangling)
	}
}

func TestWalkSessionClosure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blob := strings.Repeat("a", 64)
	rootSession := filepath.Join(root, "sessions", "stem", "stem.jsonl")
	mustMkdir(t, filepath.Join(root, "sessions", "stem"))
	mustWrite(t, rootSession, "{\"type\":\"session\",\"id\":\"session-a\",\"blob\":\"blob:sha256:"+blob+"\"}\n")
	mustWrite(t, filepath.Join(root, "sessions", "stem", "7.webp"), "artifact")
	mustMkdir(t, filepath.Join(root, "blobs"))
	mustWrite(t, filepath.Join(root, "blobs", blob), "not matching content")
	got, _, err := Walk(config.Walk{Root: root, Sources: []config.Source{
		{Name: "sessions", Path: "sessions"}, {Name: "blobs", Path: "blobs"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Dangling) != 1 || got.Dangling[0].Reason != "digest-mismatch" {
		t.Fatalf("dangling = %#v, want digest-mismatch", got.Dangling)
	}
	if !hasLine(got.Lines, rootSession, "omp-session/session-a") {
		t.Fatalf("root session record missing: %#v", got.Lines)
	}
}

func TestSince(t *testing.T) {
	t.Parallel()
	old := Line{Root: "root", Path: "same", Type: "file", Size: 1, MtimeNs: 1, SHA256: sha256.Sum256([]byte("a"))}
	newerMtime := old
	newerMtime.MtimeNs = 100
	changed := old
	changed.Size = 2
	changed.SHA256 = sha256.Sum256([]byte("bb"))
	added := Line{Root: "root", Path: "added", Type: "file", Size: 0}
	got := Since(Inventory{Lines: []Line{old}}, Inventory{Lines: []Line{newerMtime, changed, added}})
	if len(got) != 2 || got[0].Path != "added" || got[1].Path != "same" {
		t.Fatalf("Since = %#v, want added and changed, ignoring mtime", got)
	}
}

func TestContentDigest(t *testing.T) {
	t.Parallel()
	first := Inventory{
		Lines:    []Line{{Path: "transcript.jsonl", Type: "file", Size: 10, SHA256: sha256.Sum256([]byte("b"))}},
		Excluded: []Excluded{{Count: 2, Bytes: 20}},
	}
	changedMaterialized := first
	changedMaterialized.Excluded = []Excluded{{Count: 1, Bytes: 20}}
	if ContentDigest(first) != ContentDigest(changedMaterialized) {
		t.Fatal("excluded materialized config changed content digest")
	}
	changedTranscript := changedMaterialized
	changedTranscript.Lines = append([]Line(nil), changedMaterialized.Lines...)
	changedTranscript.Lines[0].SHA256 = sha256.Sum256([]byte("changed"))
	if ContentDigest(first) == ContentDigest(changedTranscript) {
		t.Fatal("transcript content did not change content digest")
	}
}

func TestWalkReusesPreviousHash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "source", "file")
	mustWrite(t, path, "content")
	cfg := config.Walk{Root: root, Sources: []config.Source{{Name: "source", Path: "source"}}}
	first, _, err := Walk(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Walk(cfg, &first)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Lines) != 1 || second.Lines[0].SHA256 != sha256.Sum256([]byte("content")) {
		t.Fatalf("previous digest not reused: %#v", second.Lines)
	}
}

func TestWalkAbsolutePathsInWatchRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".omp", "source", "file")
	mustWrite(t, path, "content")
	cfg := config.Walk{
		Root:    filepath.Join(root, "."),
		Sources: []config.Source{{Name: "source", Path: ".omp/source"}},
		Watch:   []string{".omp"},
	}
	got, _, err := Walk(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 0 || len(got.Lines) != 1 || got.Lines[0].Path != path {
		t.Fatalf("walk = %#v", got)
	}
}

func hasLine(lines []Line, path, record string) bool {
	for _, line := range lines {
		if line.Path == path && line.Record == record {
			return true
		}
	}
	return false
}

func hasType(lines []Line, path, kind string) bool {
	for _, line := range lines {
		if line.Path == path && line.Type == kind {
			return true
		}
	}
	return false
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// layoutHost is the synthetic host in testdata/archive.cue whose layout the walk tests exercise.
const layoutHost = "example-host"

// sharedSourceFiles holds one file under each shared source in testdata/archive.cue.
var sharedSourceFiles = []string{
	".example/agent/sessions/session.log",
	".example/agent/blobs/0123456789abcdef",
	".example/agent/custom-session-files/marker",
	".example/logs/example.log",
	".example/agent/agent.db",
	".example/agent/agent.db-wal",
	".example/agent/agent.db-shm",
}

// excludedFiles are classified exclusions under the watched .example root.
var excludedFiles = []string{
	".example/cache/github-cache.db",
	".example/install-id",
}

// excludedSymlinks are config links under .example/agent that the walk must skip.
var excludedSymlinks = []string{
	".example/agent/config.yml",
}

func TestLayoutHostHasNoWatchRootErrors(t *testing.T) {
	t.Parallel()
	got := walkLayoutHost(t, layoutHost, buildLayoutHome(t))
	for _, assignment := range got.Errors {
		if assignment.Reason == "unknown watched file" {
			t.Errorf("watch-root error on %s", assignment.Path)
		}
	}
}

func TestLayoutHostWalksClassifiedEntries(t *testing.T) {
	t.Parallel()
	got := walkLayoutHost(t, layoutHost, buildLayoutHome(t))
	for _, rel := range sharedSourceFiles {
		if !hasPathSuffix(got.Lines, "/"+rel) {
			t.Errorf("source %s has no inventory line", rel)
		}
	}
	for _, group := range [][]string{excludedFiles, excludedSymlinks} {
		for _, rel := range group {
			if hasPathSuffix(got.Lines, "/"+rel) {
				t.Errorf("%s was walked", rel)
			}
		}
	}
}

func TestLayoutHostFlagsUnlistedEntry(t *testing.T) {
	t.Parallel()
	home := buildLayoutHome(t)
	mustWrite(t, filepath.Join(home, ".example", "unlisted-store", "data"), "unlisted")
	got := walkLayoutHost(t, layoutHost, home)
	want := filepath.ToSlash(filepath.Join(home, ".example", "unlisted-store", "data"))
	found := false
	for _, assignment := range got.Errors {
		if assignment.Reason == "unknown watched file" && assignment.Path == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no watch-root error for unlisted %s; errors = %#v", want, got.Errors)
	}
	if !hasPathSuffix(got.Lines, "/.example/agent/agent.db") {
		t.Error("walk lost source lines after an unlisted entry")
	}
}

func TestLayoutHostMarksFinalOnlyDatabases(t *testing.T) {
	t.Parallel()
	got := walkLayoutHost(t, layoutHost, buildLayoutHome(t))
	for _, rel := range []string{".example/agent/agent.db", ".example/agent/agent.db-wal", ".example/agent/agent.db-shm"} {
		line, ok := findPathSuffix(got.Lines, "/"+rel)
		if !ok {
			t.Fatalf("missing final-only database %s", rel)
		}
		if line.Class != "final-only" {
			t.Errorf("%s class = %q, want final-only", rel, line.Class)
		}
	}
}

// walkLayoutHost runs the synthetic testdata host entry against a synthetic home.
func walkLayoutHost(t *testing.T, host, home string) Inventory {
	t.Helper()
	loaded, err := config.Load(filepath.Join("..", "..", "testdata", "archive.cue"))
	if err != nil {
		t.Fatal(err)
	}
	walk, err := loaded.Walk(host)
	if err != nil {
		t.Fatal(err)
	}
	walk.Root = home
	got, _, err := Walk(walk, nil)
	if err != nil {
		t.Fatalf("%s: walk aborted: %v", host, err)
	}
	return got
}

// buildLayoutHome writes every path the layout names; it never writes an unlisted entry.
func buildLayoutHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, group := range [][]string{sharedSourceFiles, excludedFiles} {
		for _, rel := range group {
			mustWrite(t, filepath.Join(home, filepath.FromSlash(rel)), "fixture")
		}
	}
	for _, rel := range excludedSymlinks {
		link := filepath.Join(home, filepath.FromSlash(rel))
		mustMkdir(t, filepath.Dir(link))
		if err := os.Symlink("/nix/store/fixture", link); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func hasPathSuffix(lines []Line, suffix string) bool {
	_, ok := findPathSuffix(lines, suffix)
	return ok
}

func findPathSuffix(lines []Line, suffix string) (Line, bool) {
	for _, line := range lines {
		if strings.HasSuffix(line.Path, suffix) {
			return line, true
		}
	}
	return Line{}, false
}
