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
		{name: "trailing slash", home: "/Users/example/", wantErr: true},
		{name: "valid", home: "/Users/example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{SourceHome: tc.home, MacUser: "example-user"}
			if err := opts.validateSourceHome(); (err != nil) != tc.wantErr {
				t.Fatalf("validateSourceHome(%q) error = %v, wantErr %v", tc.home, err, tc.wantErr)
			}
			if err := opts.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%q) error = %v, wantErr %v", tc.home, err, tc.wantErr)
			}
		})
	}
}

func TestStageCredentialsScriptOwner(t *testing.T) {
	script := stageCredentialsScript(Options{SourceHome: "/Users/example", MacUser: "example-user"})
	if want := "owner='example-user:staff'"; !strings.Contains(script, want) {
		t.Fatalf("script lacks %q:\n%s", want, script)
	}
}
