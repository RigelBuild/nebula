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
	if want := "'example-user:staff'"; !strings.Contains(cmd, want) {
		t.Fatalf("staging command lacks owner argument %q:\n%s", want, cmd)
	}
	if !strings.Contains(cmd, " sh ") {
		t.Fatalf("staging command does not pass the script as sh -c argument:\n%s", cmd)
	}
}
