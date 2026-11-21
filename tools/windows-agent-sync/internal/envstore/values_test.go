package envstore

import "testing"

func TestValueType(t *testing.T) {
	tests := []struct {
		name     string
		existing uint32
		found    bool
		want     uint32
	}{
		{
			name:     "existing expand string stays expandable",
			existing: registryExpandString,
			found:    true,
			want:     registryExpandString,
		},
		{
			name:     "existing string stays string",
			existing: registryString,
			found:    true,
			want:     registryString,
		},
		{
			name:     "new value uses string",
			existing: registryExpandString,
			found:    false,
			want:     registryString,
		},
		{
			name:     "other existing type uses string",
			existing: 3,
			found:    true,
			want:     registryString,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := valueType(test.existing, test.found); got != test.want {
				t.Errorf("valueType(%d, %t) = %d, want %d", test.existing, test.found, got, test.want)
			}
		})
	}
}

func TestEnvironmentStagingTargetsHKCUEnvironment(t *testing.T) {
	if got, want := environmentRegistryPath, `HKCU\Environment`; got != want {
		t.Fatalf("environmentRegistryPath = %q, want %q", got, want)
	}
}
