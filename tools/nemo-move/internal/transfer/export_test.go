package transfer

import (
	"strings"
	"testing"
)

func TestOptionsValidateSourceHome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		home    string
		wantErr bool
	}{
		{name: "relative", home: "Users/example", wantErr: true},
		{name: "root", home: "/", wantErr: true},
		{name: "trailing slash", home: "/src/home/example/", wantErr: true},
		{name: "valid", home: "/src/home/example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{SourceHome: tc.home, MacUser: "example-user", Layout: testLayout}
			if err := opts.validateSourceHome(); (err != nil) != tc.wantErr {
				t.Fatalf("validateSourceHome(%q) error = %v, wantErr %v", tc.home, err, tc.wantErr)
			}
			if err := opts.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%q) error = %v, wantErr %v", tc.home, err, tc.wantErr)
			}
		})
	}
}

func TestStageShellPassesValuesAsArguments(t *testing.T) {
	cmd := stageShell(Options{SourceHome: "/src/home/example", MacUser: "example-user", Layout: testLayout})
	if strings.Contains(stageCredentialsScript, "example-user") {
		t.Fatal("staging script text contains an inventory value")
	}
	if strings.Contains(cmd, "example-user") {
		t.Fatalf("root staging command takes an owner argument; root must not chown:\n%s", cmd)
	}
	if !strings.Contains(cmd, " sh ") {
		t.Fatalf("staging command does not pass the script as sh -c argument:\n%s", cmd)
	}
}

// The Go side splices nothing: each account reaches the command as its own
// quoted argument. A revert that joins raw account names into the command would
// leave a hostile name unquoted, and this test would fail.
func TestStageShellQuotesAccountNames(t *testing.T) {
	hostile := []string{"$(id)", "a'b", "x;rm -rf ~"}
	lay := testLayout
	lay.Google.Accounts = hostile
	cmd := stageShell(Options{SourceHome: "/src/home/example", MacUser: "example-user", Layout: lay})
	for _, account := range hostile {
		if !strings.Contains(cmd, ShellQuote(account)) {
			t.Fatalf("staging command does not pass account %q as a quoted argument:\n%s", account, cmd)
		}
		if strings.Contains(stageCredentialsScript, account) {
			t.Fatalf("staging script text contains account %q", account)
		}
	}
}
