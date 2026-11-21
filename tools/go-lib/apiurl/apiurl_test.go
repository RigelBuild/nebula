package apiurl

import "testing"

func envOf(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

const inCluster = "https://ci.example.com"

func TestWoodpeckerAPI(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
		ok   bool
	}{
		{"WOODPECKER_API_URL wins over Kubernetes and CI_SYSTEM_URL",
			map[string]string{"WOODPECKER_API_URL": "https://api.ci", "KUBERNETES_SERVICE_HOST": "10.0.0.1", "CI_SYSTEM_URL": "https://builtin.ci"},
			"https://api.ci", true},
		{"Kubernetes service wins over CI_SYSTEM_URL",
			map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "CI_SYSTEM_URL": "https://builtin.ci"},
			inCluster, true},
		{"CI_SYSTEM_URL is used outside Kubernetes",
			map[string]string{"CI_SYSTEM_URL": "https://builtin.ci"},
			"https://builtin.ci", true},
		{"KUBERNETES_SERVICE_HOST is absent with CI_SYSTEM_URL",
			map[string]string{"KUBERNETES_SERVICE_HOST": "", "CI_SYSTEM_URL": "https://builtin.ci"},
			"https://builtin.ci", true},
		{"returns none when every source is absent", map[string]string{}, "", false},
		{"trims WOODPECKER_API_URL and strips trailing slashes",
			map[string]string{"WOODPECKER_API_URL": "  https://api.ci///  "},
			"https://api.ci", true},
		{"trims CI_SYSTEM_URL and strips trailing slashes",
			map[string]string{"CI_SYSTEM_URL": "\thttps://builtin.ci///\n"},
			"https://builtin.ci", true},
		{"an empty API URL falls through to Kubernetes",
			map[string]string{"WOODPECKER_API_URL": "", "KUBERNETES_SERVICE_HOST": "10.0.0.1"},
			inCluster, true},
		{"a whitespace API URL falls through to CI_SYSTEM_URL outside Kubernetes",
			map[string]string{"WOODPECKER_API_URL": "  \t", "CI_SYSTEM_URL": "https://builtin.ci"},
			"https://builtin.ci", true},
		{"a whitespace Kubernetes host counts as absent",
			map[string]string{"KUBERNETES_SERVICE_HOST": " \t ", "CI_SYSTEM_URL": "https://builtin.ci"},
			"https://builtin.ci", true},
		{"empty API and CI URLs with no Kubernetes host return none",
			map[string]string{"WOODPECKER_API_URL": " ", "CI_SYSTEM_URL": ""},
			"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := WoodpeckerAPI(envOf(tc.env), inCluster)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("WoodpeckerAPI = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWoodpeckerAPIInClusterFromCaller(t *testing.T) {
	env := envOf(map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "CI_SYSTEM_URL": "https://builtin.ci"})
	if got, ok := WoodpeckerAPI(env, "https://ci.example.com/"); got != "https://ci.example.com" || !ok {
		t.Fatalf("WoodpeckerAPI = (%q, %v), want the caller's in-cluster URL", got, ok)
	}
	for _, empty := range []string{"", " "} {
		if got, ok := WoodpeckerAPI(env, empty); got != "" || ok {
			t.Fatalf("WoodpeckerAPI in cluster with in-cluster URL %q = (%q, %v), want none", empty, got, ok)
		}
	}
	outside := envOf(map[string]string{"CI_SYSTEM_URL": "https://builtin.ci"})
	if got, ok := WoodpeckerAPI(outside, ""); got != "https://builtin.ci" || !ok {
		t.Fatalf("WoodpeckerAPI outside cluster = (%q, %v), want CI_SYSTEM_URL", got, ok)
	}
}
