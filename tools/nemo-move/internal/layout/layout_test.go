package layout

import (
	"strings"
	"testing"
)

// validLayout is a minimal layout that Parse accepts.
const validLayout = `
rows: [
	{path: ".example-agent", kind: "copy", dest: "/opt/data", skip: ["gateway.pid"]},
	{path: "Library/Cache", kind: "rebuild", dest: "/opt/data/cache"},
	{path: "Library/Old", kind: "retire", dest: "none"},
]
google: {source: "/var/lib/example-google/accounts", dest: "/opt/data/.local/state/example-google", accounts: ["workspace"]}
journals: {dir: "/var/lib/example-journals", names: ["pages.jsonl"]}
processes: ["example-agent --serve"]
`

func TestParseRoundTripsThroughJSON(t *testing.T) {
	lay, err := Parse([]byte(validLayout))
	if err != nil {
		t.Fatal(err)
	}
	text, err := lay.JSON()
	if err != nil {
		t.Fatal(err)
	}
	again, err := FromJSON(text)
	if err != nil {
		t.Fatalf("FromJSON(JSON()) error = %v", err)
	}
	if len(again.Rows) != len(lay.Rows) {
		t.Fatalf("rows after round trip = %d, want %d", len(again.Rows), len(lay.Rows))
	}
	if again.Google.Dest != lay.Google.Dest || again.Journals.Dir != lay.Journals.Dir {
		t.Fatalf("round trip changed google or journals: %+v %+v", again.Google, again.Journals)
	}
}

func TestParseAddsOneStagingRowPerAccount(t *testing.T) {
	lay, err := Parse([]byte(validLayout))
	if err != nil {
		t.Fatal(err)
	}
	want := StagingDir + "/workspace/credentials"
	var found bool
	for _, row := range lay.Rows {
		if row.Path == want && row.Kind == Copy {
			found = true
		}
	}
	if !found {
		t.Fatalf("no staging copy row %s in %+v", want, lay.Rows)
	}
}

func TestParseRejectsUnknownKind(t *testing.T) {
	bad := strings.Replace(validLayout, `kind: "rebuild"`, `kind: "bogus"`, 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("Parse accepted an unknown row kind")
	}
}

func TestParseRejectsCopyOutsideDataRoot(t *testing.T) {
	bad := strings.Replace(validLayout, `dest: "/opt/data"`, `dest: "/etc"`, 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("Parse accepted a copy destination outside /opt/data")
	}
}

func TestFromJSONRejectsUnknownField(t *testing.T) {
	if _, err := FromJSON(`{"rows":[],"surprise":true}`); err == nil {
		t.Fatal("FromJSON accepted an unknown field")
	}
}

func TestFromJSONRejectsBadAccountName(t *testing.T) {
	base, err := Parse([]byte(validLayout))
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"work space", "x;rm -rf ~", "$(id)", "a'b", "-leading", "a/b", "..", ""} {
		lay := base
		lay.Google.Accounts = []string{account}
		text, err := lay.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := FromJSON(text); err == nil {
			t.Fatalf("FromJSON accepted account %q", account)
		}
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load("does-not-exist.cue"); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}
