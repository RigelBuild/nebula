package cdrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifestPath is a minimal fixture of the Flux manifest that seeds the queue
// ConfigMap. The store names the object it creates.
const manifestPath = "testdata/cd-release-queue.yaml"

// The manifest is what Flux seeds from. Read the fixture so a rename or a
// changed seed state fails here, not in production.
func TestQueueConfigMapAndSeedStateMatchFluxManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(manifestPath))
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	text := string(raw)
	if !strings.Contains(text, "name: "+QueueConfigMap+"\n") {
		t.Fatalf("%s does not name ConfigMap %q", manifestPath, QueueConfigMap)
	}
	if !strings.Contains(text, "state: '"+QueueSeedState+"'") {
		t.Fatalf("%s seed state differs from QueueSeedState %q", manifestPath, QueueSeedState)
	}
}
