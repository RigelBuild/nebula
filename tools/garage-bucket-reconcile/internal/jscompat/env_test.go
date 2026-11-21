package jscompat

import "testing"

func TestRequiredTreatsBlankAsMissing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		want    string
		wantErr string
	}{
		{name: "present", value: "http://garage.example.com:3903", want: "http://garage.example.com:3903"},
		{name: "surrounding spaces kept in value", value: " token ", want: " token "},
		{name: "unset", value: "", wantErr: "GARAGE_ADMIN_URL is required"},
		{name: "blank", value: "   ", wantErr: "GARAGE_ADMIN_URL is required"},
		{name: "tab and newline only", value: "\t\n", wantErr: "GARAGE_ADMIN_URL is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(string) string { return tc.value }
			got, err := Required(getenv, "GARAGE_ADMIN_URL")
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Required error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Required: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Required = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLookupReportsBlankWithoutError(t *testing.T) {
	t.Parallel()
	value, ok := Lookup(func(string) string { return "  " }, "KUBERNETES_SERVICE_HOST")
	if ok {
		t.Fatalf("Lookup reported a blank value as present (value %q)", value)
	}
	value, ok = Lookup(func(string) string { return "10.0.0.1" }, "KUBERNETES_SERVICE_HOST")
	if !ok || value != "10.0.0.1" {
		t.Fatalf("Lookup = (%q, %v), want (10.0.0.1, true)", value, ok)
	}
}
