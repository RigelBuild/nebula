// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyTables(t *testing.T) {
	policyBytes, err := os.ReadFile("testdata/policy-table.json")
	if err != nil {
		t.Fatal(err)
	}
	entitlementBytes, err := os.ReadFile("testdata/entitlements.json")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := ParsePolicyTable(string(policyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if policies["build.coordination.lead"] != (RolePolicy{Team: "agents-build", PolicyID: "agents-build-v1"}) {
		t.Fatalf("role policy = %#v", policies["build.coordination.lead"])
	}
	if len(policies) != 2 || policies["design.architect"] != (RolePolicy{Team: "agents-design", PolicyID: "agents-design-v1"}) {
		t.Fatalf("role policies = %#v", policies)
	}
	for _, test := range []struct{ name, text, want string }{
		{"unknown role field", `{"rolePolicies":{"design.architect":{"team":"t","policyId":"p","extra":1}},"policies":{}}`, `unknown key "extra"`},
		{"missing team", `{"rolePolicies":{"role":{"policyId":"p"}},"policies":{}}`, "team must be a non-empty string"},
		{"empty policy id", `{"rolePolicies":{"role":{"team":"t","policyId":""}},"policies":{}}`, "policyId must be a non-empty string"},
		{"string row", `{"rolePolicies":{"role":"policy"},"policies":{}}`, "must be an object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParsePolicyTable(test.text)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
	testPolicyLimitsAndEntitlements(t, policyBytes, entitlementBytes)
}

func testPolicyLimitsAndEntitlements(t *testing.T, policyBytes, entitlementBytes []byte) {
	t.Helper()
	key := KeyRequest{Routing: map[Provider]any{ProviderClaude: []string{"team-build"}}, Limits: KeyLimits{KeyCap: &KeyCap{USD: 10, Window: Window{Kind: "rolling", Seconds: 86400}}, PerModel: map[string]ModelLimit{"model-a": {Tokens: new(uint64(500)), TPM: new(uint64(100)), RPM: new(uint64(10))}}}}
	if err := ValidateEntitlement(string(entitlementBytes), "lead", "build.coordination.lead", key); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePolicy(string(policyBytes), "agents-build-v1", key); err != nil {
		t.Fatal(err)
	}
	badWindow := key
	if !sameWindow(key.Limits.KeyCap.Window, Window{Kind: "rolling", Seconds: 86400}) {
		t.Fatal("test request window does not match policy fixture")
	}
	badWindow.Limits.KeyCap = &KeyCap{USD: 10, Window: Window{Kind: "rolling", Seconds: 3600}}
	if err := ValidatePolicy(string(policyBytes), "agents-build-v1", badWindow); err == nil || !strings.Contains(err.Error(), "window exceeds policy") {
		t.Fatalf("key cap window error=%v", err)
	}
	if err := ValidatePolicy(string(policyBytes), "agents-design-v1", key); err == nil || !strings.Contains(err.Error(), "model not allowed") {
		t.Fatalf("model error=%v", err)
	}
	badCap := key
	badCap.Limits.KeyCap = &KeyCap{USD: 21, Window: key.Limits.KeyCap.Window}
	if err := ValidatePolicy(string(policyBytes), "agents-build-v1", badCap); err == nil || !strings.Contains(err.Error(), "key cap exceeds") {
		t.Fatalf("key cap error=%v", err)
	}
	badModel := key
	badModel.Limits.PerModel = map[string]ModelLimit{"model-a": {TPM: new(uint64(101))}}
	if err := ValidatePolicy(string(policyBytes), "agents-build-v1", badModel); err == nil || !strings.Contains(err.Error(), "exceeds policy") {
		t.Fatalf("model limit error=%v", err)
	}
	if err := ValidateEntitlement(string(entitlementBytes), "lead", "design.architect", key); err == nil || !strings.Contains(err.Error(), "permission-denied") {
		t.Fatalf("entitlement role error=%v", err)
	}
	if err := ValidateEntitlement(string(entitlementBytes), "lead", "build.coordination.lead", KeyRequest{Routing: map[Provider]any{ProviderClaude: "shared"}}); err != nil {
		t.Fatalf("shared entitlement error=%v", err)
	}
}

func TestReadTrustedTableRejectsSymlinksAndWritableFiles(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "wave-seats.json")
	table := filepath.Join(dir, "policy-table.json")
	if err := os.WriteFile(list, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(table, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted, err := readTrustedDescriptor(table, func() uint32 {
		info, e := os.Stat(list)
		if e != nil {
			t.Fatal(e)
		}
		owner, _ := fileOwner(info)
		return owner
	}())
	if err != nil || trusted.Digest != DigestText("{}") || trusted.Text != "{}" {
		t.Fatalf("trusted=%#v err=%v", trusted, err)
	}
	if err := os.Chmod(table, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedDescriptor(table, func() uint32 {
		info, e := os.Stat(list)
		if e != nil {
			t.Fatal(e)
		}
		owner, _ := fileOwner(info)
		return owner
	}()); err == nil {
		t.Fatal("group writable table accepted")
	}
	if err := os.Chmod(table, 0o602); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedDescriptor(table, 0); err == nil {
		t.Fatal("world writable table accepted")
	}
	if _, err := readTrustedDescriptor(dir, 0); err == nil {
		t.Fatal("directory accepted")
	}
	if err := os.Chmod(table, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(table, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedDescriptor(link, 0); err == nil {
		t.Fatal("symlink accepted")
	}
}
