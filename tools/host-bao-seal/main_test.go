package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const testSecretID = "secret-id-plaintext-must-only-be-piped"

var testEncryptedYAML = []byte("secret-id: ENC[AGE,data:opaque,type:str]\nsops:\n  mac: opaque\n")

type fakeStackOutputReader struct {
	ids   secretIDsByHost
	stack string
	err   error
}

func (reader *fakeStackOutputReader) ReadSecretIDs(
	_ context.Context,
	stack string,
) (secretIDsByHost, error) {
	reader.stack = stack
	return reader.ids, reader.err
}

type fakeSecretEncryptor struct {
	plaintext  []byte
	recipients string
	env        []string
	sealed     []byte
	err        error
}

func (encryptor *fakeSecretEncryptor) Encrypt(
	_ context.Context,
	plaintext io.Reader,
	recipients string,
	env []string,
) ([]byte, error) {
	if encryptor.err != nil {
		return nil, encryptor.err
	}
	data, err := io.ReadAll(plaintext)
	if err != nil {
		return nil, err
	}
	encryptor.plaintext = data
	encryptor.recipients = recipients
	encryptor.env = append([]string{}, env...)
	return bytes.Clone(encryptor.sealed), nil
}

type childProbeRunner struct {
	binary string
}

func (runner childProbeRunner) Run(
	ctx context.Context,
	name string,
	args []string,
	stdin io.Reader,
	env []string,
) ([]byte, error) {
	if name != "pulumi" && name != "sops" {
		return nil, fmt.Errorf("unexpected command %q", name)
	}
	commandArgs := make([]string, 0, 3+len(args))
	commandArgs = append(commandArgs, "-test.run=TestCommandChildProcess", "--", name)
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, runner.binary, commandArgs...)
	command.Env = slices.Clone(env)
	command.Env = append(command.Env, "HOST_BAO_SEAL_TEST_CHILD=1")
	command.Stdin = stdin
	return command.Output()
}

func writeSopsConfig(t *testing.T, directory, host, recipients string) string {
	t.Helper()
	secretsDir := filepath.Join(directory, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatalf("create SOPS output directory: %v", err)
	}
	config := fmt.Sprintf(
		"creation_rules:\n  - path_regex: secrets/%s\\.yaml$\n    age: %q\n",
		strings.ReplaceAll(host+"-bao", ".", "\\."),
		recipients,
	)
	if err := os.WriteFile(filepath.Join(directory, ".sops.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write SOPS config: %v", err)
	}
	return filepath.Join(secretsDir, host+"-bao.yaml")
}

func TestHostBaoSeal(t *testing.T) {
	tempDir := t.TempDir()
	const recipients = "age1operator,age1examplehost"
	parsed := options{
		stack:      "example-org/prod",
		pulumiDir:  "infra/pulumi/secrets",
		host:       "example-host",
		out:        writeSopsConfig(t, tempDir, "example-host", recipients),
		generation: 3,
	}
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "plaintext is removed from encryptor environment", run: func(t *testing.T) { testHostBaoSealPlaintextRemoved(t, parsed) }},
		{name: "no plaintext in argv or env of any child process", run: func(t *testing.T) { testHostBaoSealChildProcess(t, parsed) }},
		{name: "CLI requires generation and rejects recipients flag", run: func(t *testing.T) { testHostBaoSealCLI(t, parsed, recipients) }},
		{name: "CLI requires pulumi-dir", run: func(t *testing.T) { testHostBaoSealPulumiDirRequired(t, parsed) }},
		{name: "output basename must match host", run: func(t *testing.T) { testHostBaoSealOutputBasename(t, tempDir) }},
		{name: "SOPS matching rule supplies recipients", run: func(t *testing.T) { testHostBaoSealMatchingRule(t, parsed, recipients) }},
		{name: "rule for another host is not used", run: func(t *testing.T) { testHostBaoSealOtherHostRule(t, tempDir, recipients) }},
		{name: "requested generation must be newest", run: func(t *testing.T) { testHostBaoSealNewestGeneration(t, parsed) }},
		{name: "unknown host fails", run: func(t *testing.T) { testHostBaoSealUnknownHost(t, parsed) }},
		{name: "encrypt failure preserves existing output", run: func(t *testing.T) { testHostBaoSealEncryptFailure(t, parsed) }},
		{name: "printed hash matches sealed plaintext", run: func(t *testing.T) { testHostBaoSealPrintedHash(t, parsed) }},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestWriteFileAtomicRenameFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	parsed := options{out: filepath.Join(dir, "existing")}
	if err := os.Mkdir(parsed.out, 0o700); err != nil {
		t.Fatalf("create target directory: %v", err)
	}

	if err := writeFileAtomic(parsed.out, []byte("replacement")); err == nil {
		t.Fatal("writeFileAtomic() succeeded when target is a directory")
	}

	entries, err := os.ReadDir(filepath.Dir(parsed.out))
	if err != nil {
		t.Fatalf("read output directory: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("temporary output file remains: %s", entry.Name())
		}
	}
}

func testHostBaoSealPlaintextRemoved(t *testing.T, parsed options) {
	t.Helper()
	reader := &fakeStackOutputReader{ids: secretIDsByHost{"example-host": {3: testSecretID}}}
	encryptor := &fakeSecretEncryptor{sealed: testEncryptedYAML}
	secretEnv := []string{"SAFE=value", "BAO_SECRET_ID=" + testSecretID}
	var stdout, stderr bytes.Buffer
	if err := runSeal(context.Background(), parsed, reader, encryptor, secretEnv, &stdout, &stderr); err != nil {
		t.Fatalf("runSeal() error = %v", err)
	}
	if !slices.Contains(encryptor.env, "SAFE=value") {
		t.Fatal("encryptor environment lost unrelated variable")
	}
	if slices.Contains(encryptor.env, "BAO_SECRET_ID="+testSecretID) {
		t.Fatal("plaintext secret_id reached encryptor environment")
	}
}

func testHostBaoSealChildProcess(t *testing.T, parsed options) {
	t.Helper()
	runner := childProbeRunner{binary: os.Args[0]}
	var stdout, stderr bytes.Buffer
	args := []string{
		"--stack", parsed.stack,
		"--pulumi-dir", parsed.pulumiDir,
		"--host", parsed.host,
		"--out", parsed.out,
		"--generation", "1",
	}
	if err := execute(
		context.Background(),
		args,
		runner,
		[]string{"HOST_BAO_SEAL_TEST_CHILD=1"},
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("execute() error = %v", err)
	}
}

func testHostBaoSealCLI(t *testing.T, parsed options, recipients string) {
	t.Helper()
	baseArgs := []string{
		"--stack", parsed.stack,
		"--pulumi-dir", parsed.pulumiDir,
		"--host", parsed.host,
		"--out", parsed.out,
	}
	if _, err := parseArgs(baseArgs); err == nil || !strings.Contains(err.Error(), "--generation") {
		t.Fatalf("parseArgs() error = %v, want required generation", err)
	}
	withGeneration := append(slices.Clone(baseArgs), "--generation", "1")
	withRecipients := slices.Concat(withGeneration, []string{"--recipients", recipients})
	if _, err := parseArgs(withRecipients); err == nil {
		t.Fatal("parseArgs() accepted removed --recipients flag")
	}
}

func testHostBaoSealPulumiDirRequired(t *testing.T, parsed options) {
	t.Helper()
	args := []string{"--stack", parsed.stack, "--host", parsed.host, "--out", parsed.out, "--generation", "1"}
	if _, err := parseArgs(args); err == nil || !strings.Contains(err.Error(), "--pulumi-dir") {
		t.Fatalf("parseArgs() error = %v, want required --pulumi-dir", err)
	}
}

func testHostBaoSealOutputBasename(t *testing.T, tempDir string) {
	t.Helper()
	wrongOut := filepath.Join(tempDir, "other-host-bao.yaml")
	reader := &fakeStackOutputReader{ids: secretIDsByHost{"example-host": {1: testSecretID}}}
	encryptor := &fakeSecretEncryptor{}
	var stdout, stderr bytes.Buffer
	err := runSeal(context.Background(), options{
		stack: "example-org/prod", pulumiDir: "infra/pulumi/secrets", host: "example-host", out: wrongOut, generation: 1,
	}, reader, encryptor, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "basename") {
		t.Fatalf("runSeal() error = %v, want host/output mismatch", err)
	}
	if encryptor.plaintext != nil {
		t.Fatal("encryptor received plaintext for mismatched output")
	}
}

func testHostBaoSealMatchingRule(t *testing.T, parsed options, recipients string) {
	t.Helper()
	reader := &fakeStackOutputReader{ids: secretIDsByHost{
		"example-host": {1: "old-secret-id", 3: testSecretID, 2: "middle-secret-id"},
	}}
	encryptor := &fakeSecretEncryptor{sealed: testEncryptedYAML}
	var stdout, stderr bytes.Buffer
	if err := runSeal(context.Background(), parsed, reader, encryptor, nil, &stdout, &stderr); err != nil {
		t.Fatalf("runSeal() error = %v", err)
	}
	if encryptor.recipients != recipients {
		t.Fatalf("encryption recipients = %q, want SOPS rule recipients %q", encryptor.recipients, recipients)
	}
	var plaintext struct {
		SecretID string `yaml:"secret-id"`
	}
	if err := yaml.Unmarshal(encryptor.plaintext, &plaintext); err != nil {
		t.Fatalf("decode encryption input: %v", err)
	}
	if plaintext.SecretID != testSecretID {
		t.Fatalf("secret_id = %q, want newest generation value", plaintext.SecretID)
	}
	if stderr.String() != "generation=3\n" {
		t.Fatalf("generation output = %q, want generation=3", stderr.String())
	}
}

func testHostBaoSealOtherHostRule(t *testing.T, tempDir, recipients string) {
	t.Helper()
	otherDir := filepath.Join(tempDir, "other")
	otherOut := writeSopsConfig(t, otherDir, "other-host", recipients)
	reader := &fakeStackOutputReader{ids: secretIDsByHost{"example-host": {1: testSecretID}}}
	encryptor := &fakeSecretEncryptor{}
	var stdout, stderr bytes.Buffer
	err := runSeal(context.Background(), options{
		stack: "example-org/prod", host: "example-host", out: filepath.Join(filepath.Dir(otherOut), "example-host-bao.yaml"),
		generation: 1,
	}, reader, encryptor, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "no SOPS creation rule") {
		t.Fatalf("runSeal() error = %v, want unmatched SOPS rule failure", err)
	}
	if encryptor.plaintext != nil {
		t.Fatal("encryptor received plaintext without a matching rule")
	}
}

func testHostBaoSealNewestGeneration(t *testing.T, parsed options) {
	t.Helper()
	reader := &fakeStackOutputReader{ids: secretIDsByHost{"example-host": {1: testSecretID}}}
	encryptor := &fakeSecretEncryptor{}
	var stdout, stderr bytes.Buffer
	err := runSeal(context.Background(), options{
		stack: parsed.stack, pulumiDir: parsed.pulumiDir, host: parsed.host, out: parsed.out, generation: 2,
	}, reader, encryptor, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not newest") {
		t.Fatalf("runSeal() error = %v, want missing generation failure", err)
	}
	if encryptor.plaintext != nil {
		t.Fatal("encryptor received plaintext for a missing generation")
	}
}

func testHostBaoSealUnknownHost(t *testing.T, parsed options) {
	t.Helper()
	reader := &fakeStackOutputReader{ids: secretIDsByHost{}}
	encryptor := &fakeSecretEncryptor{}
	var stdout, stderr bytes.Buffer
	err := runSeal(context.Background(), parsed, reader, encryptor, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unknown host") {
		t.Fatalf("runSeal() error = %v, want unknown host failure", err)
	}
	if encryptor.plaintext != nil {
		t.Fatal("encryptor received plaintext for an unknown host")
	}
}

func testHostBaoSealEncryptFailure(t *testing.T, parsed options) {
	t.Helper()
	if err := os.WriteFile(parsed.out, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("write existing output: %v", err)
	}
	reader := &fakeStackOutputReader{ids: secretIDsByHost{"example-host": {3: testSecretID}}}
	encryptor := &fakeSecretEncryptor{err: errors.New("encryption failed")}
	var stdout, stderr bytes.Buffer
	if err := runSeal(context.Background(), parsed, reader, encryptor, nil, &stdout, &stderr); err == nil {
		t.Fatal("runSeal() succeeded, want encryption failure")
	}
	got, err := os.ReadFile(parsed.out)
	if err != nil {
		t.Fatalf("read existing output: %v", err)
	}
	if string(got) != "sentinel" {
		t.Fatalf("existing output = %q, want sentinel", got)
	}
}

func testHostBaoSealPrintedHash(t *testing.T, parsed options) {
	t.Helper()
	reader := &fakeStackOutputReader{ids: secretIDsByHost{"example-host": {3: testSecretID}}}
	encryptor := &fakeSecretEncryptor{sealed: testEncryptedYAML}
	var stdout, stderr bytes.Buffer
	if err := runSeal(context.Background(), parsed, reader, encryptor, nil, &stdout, &stderr); err != nil {
		t.Fatalf("runSeal() error = %v", err)
	}
	sealed, err := os.ReadFile(parsed.out)
	if err != nil {
		t.Fatalf("read sealed output: %v", err)
	}
	if !bytes.Equal(sealed, testEncryptedYAML) {
		t.Fatalf("sealed output = %q, want encrypted YAML", sealed)
	}
	hash := sha256.Sum256([]byte(testSecretID))
	want := hex.EncodeToString(hash[:]) + "\n"
	if stdout.String() != want {
		t.Fatalf("printed hash = %q, want %q", stdout.String(), want)
	}
}

func TestNewestSecretIDRejectsEmptyValues(t *testing.T) {
	tests := []struct {
		name string
		ids  secretIDsByHost
	}{
		{
			name: "no generations",
			ids:  secretIDsByHost{"example-host": {}},
		},
		{
			name: "empty secret id",
			ids:  secretIDsByHost{"example-host": {1: ""}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := newestSecretID(test.ids, "example-host"); err == nil {
				t.Fatal("newestSecretID() succeeded, want failure")
			}
		})
	}
}

func TestSopsAgeRoundTrip(t *testing.T) {
	if os.Getenv("HOST_BAO_SEAL_SKIP_SOPS_TEST") == "1" {
		t.Skip("disabled by HOST_BAO_SEAL_SKIP_SOPS_TEST=1")
	}
	sopsPath, err := exec.LookPath("sops")
	if err != nil {
		t.Fatalf("locate sops (set HOST_BAO_SEAL_SKIP_SOPS_TEST=1 to opt out): %v", err)
	}
	ageKeygenPath, err := exec.LookPath("age-keygen")
	if err != nil {
		t.Fatalf("locate age-keygen (set HOST_BAO_SEAL_SKIP_SOPS_TEST=1 to opt out): %v", err)
	}

	tempDir := t.TempDir()
	testEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tempDir}
	identities := []string{
		filepath.Join(tempDir, "first-identity.txt"),
		filepath.Join(tempDir, "second-identity.txt"),
	}
	recipients := make([]string, 0, len(identities))
	for _, identity := range identities {
		generate := exec.CommandContext(t.Context(), ageKeygenPath, "-o", identity)
		generate.Env = testEnv
		if err := generate.Run(); err != nil {
			t.Fatalf("generate age identity: %v", err)
		}

		publicKey := exec.CommandContext(t.Context(), ageKeygenPath, "-y", identity)
		publicKey.Env = testEnv
		output, err := publicKey.Output()
		if err != nil {
			t.Fatalf("read age recipient: %v", err)
		}
		recipient := strings.TrimSpace(string(output))
		if recipient == "" {
			t.Fatal("age-keygen returned an empty recipient")
		}
		recipients = append(recipients, recipient)
	}

	parsed := options{
		stack:      "example-org/prod",
		pulumiDir:  "infra/pulumi/secrets",
		host:       "example-host",
		out:        writeSopsConfig(t, tempDir, "example-host", strings.Join(recipients, ",")),
		generation: 1,
	}
	reader := &fakeStackOutputReader{ids: secretIDsByHost{
		parsed.host: {1: testSecretID},
	}}
	var stdout, stderr bytes.Buffer
	if err := runSeal(
		t.Context(),
		parsed,
		reader,
		sopsEncryptor{runner: execCommandRunner{}},
		testEnv,
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runSeal() with real sops: %v", err)
	}

	for index, identity := range identities {
		t.Run(fmt.Sprintf("decrypt with recipient %d", index+1), func(t *testing.T) {
			decrypt := exec.CommandContext(
				t.Context(),
				sopsPath,
				"--decrypt",
				"--input-type", "yaml",
				"--output-type", "yaml",
				parsed.out,
			)
			decrypt.Env = slices.Clone(testEnv)
			decrypt.Env = append(decrypt.Env, "SOPS_AGE_KEY_FILE="+identity)
			plaintext, err := decrypt.Output()
			if err != nil {
				t.Fatalf("decrypt with age identity: %v", err)
			}
			var decoded struct {
				SecretID string `yaml:"secret-id"`
			}
			if err := yaml.Unmarshal(plaintext, &decoded); err != nil {
				t.Fatalf("decode decrypted YAML: %v", err)
			}
			if decoded.SecretID != testSecretID {
				t.Fatal("decrypted secret-id did not round-trip")
			}
		})
	}
}

func TestCommandChildProcess(t *testing.T) {
	if os.Getenv("HOST_BAO_SEAL_TEST_CHILD") != "1" {
		t.Skip("child-process probe")
	}
	if len(os.Args) < 4 {
		t.Fatalf("child argv does not identify command: %v", os.Args)
	}
	commandName := os.Args[3]
	for _, argument := range os.Args {
		if strings.Contains(argument, testSecretID) {
			t.Fatalf("plaintext reached %s argv", commandName)
		}
	}
	for _, entry := range os.Environ() {
		if strings.Contains(entry, testSecretID) {
			t.Fatalf("plaintext reached %s environment", commandName)
		}
	}
	if commandName == "pulumi" {
		if _, err := os.Stdout.Write([]byte(`{"example-host":{"1":"secret-id-plaintext-must-only-be-piped"}}`)); err != nil {
			t.Fatalf("write mock Pulumi output: %v", err)
		}
		os.Exit(0)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatalf("read child stdin: %v", err)
	}
	if !bytes.Contains(input, []byte(testSecretID)) {
		t.Fatal("sops stdin does not contain the piped plaintext")
	}
	if _, err := os.Stdout.Write(testEncryptedYAML); err != nil {
		t.Fatalf("write fake sops ciphertext: %v", err)
	}
	os.Exit(0)
}

type recordingCommandRunner struct {
	name string
	args []string
}

func (runner *recordingCommandRunner) Run(
	_ context.Context,
	name string,
	args []string,
	_ io.Reader,
	_ []string,
) ([]byte, error) {
	runner.name = name
	runner.args = slices.Clone(args)
	return []byte(`{"example-host":{"1":"secret-id"}}`), nil
}

func TestPulumiStackOutputReaderPassesPulumiDir(t *testing.T) {
	runner := &recordingCommandRunner{}
	reader := pulumiStackOutputReader{runner: runner, pulumiDir: "infra/pulumi/secrets"}
	if _, err := reader.ReadSecretIDs(t.Context(), "example-org/prod"); err != nil {
		t.Fatalf("ReadSecretIDs() error = %v", err)
	}
	if runner.name != "pulumi" {
		t.Fatalf("command = %q, want pulumi", runner.name)
	}
	want := []string{
		"stack", "output", "secretIdsByHost",
		"--json", "--show-secrets",
		"--stack", "example-org/prod",
		"-C", "infra/pulumi/secrets",
	}
	if !slices.Equal(runner.args, want) {
		t.Fatalf("pulumi args = %q, want %q", runner.args, want)
	}
}
