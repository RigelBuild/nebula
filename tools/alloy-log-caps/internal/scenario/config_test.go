package scenario

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournalBodyDropsFirstForwardTo(t *testing.T) {
	config := "loki.process \"journal\" {\n  forward_to = [a]\n  stage.x {}\n}\n"
	got, err := journalBody(config)
	if err != nil {
		t.Fatalf("journalBody: %v", err)
	}
	if want := "\n  stage.x {}"; got != want {
		t.Errorf("journalBody = %q, want %q", got, want)
	}
}

func TestJournalBodyMissingBlock(t *testing.T) {
	if _, err := journalBody("loki.source.journal \"x\" {}\n"); err == nil {
		t.Error("journalBody with no journal block returned nil error")
	}
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "k8s.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func TestYAMLStageReadsString(t *testing.T) {
	path := writeYAML(t, "spec:\n  values:\n    stages: \"stage.cri {}\"\n")
	got, err := yamlStage(path, "spec", "values", "stages")
	if err != nil {
		t.Fatalf("yamlStage: %v", err)
	}
	if got != "stage.cri {}" {
		t.Errorf("yamlStage = %q, want stage.cri {}", got)
	}
}

func TestYAMLStageMissingKey(t *testing.T) {
	path := writeYAML(t, "spec:\n  values: {}\n")
	if _, err := yamlStage(path, "spec", "values", "stages"); err == nil {
		t.Error("yamlStage on a missing key returned nil error")
	}
}

func TestYAMLStageNonStringValue(t *testing.T) {
	path := writeYAML(t, "spec:\n  values:\n    stages: 42\n")
	if _, err := yamlStage(path, "spec", "values", "stages"); err == nil {
		t.Error("yamlStage on a non-string value returned nil error")
	}
}

func TestNestedStopsAtNonMap(t *testing.T) {
	doc := map[string]any{"a": "leaf"}
	if _, ok := nested(doc, "a", "b"); ok {
		t.Error("nested walked through a string value")
	}
	if _, ok := nested(doc, "missing"); ok {
		t.Error("nested found a missing key")
	}
}
