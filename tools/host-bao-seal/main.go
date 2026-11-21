package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Invoke fixed Pulumi and SOPS executables with argv, without a shell.
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type secretIDsByHost map[string]map[int]string

type stackOutputReader interface {
	ReadSecretIDs(ctx context.Context, stack string) (secretIDsByHost, error)
}

type commandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, env []string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(
	ctx context.Context,
	name string,
	args []string,
	stdin io.Reader,
	env []string,
) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // Fixed executables; arguments do not pass through a shell.
	command.Stdin = stdin
	if env != nil {
		command.Env = env
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return nil, fmt.Errorf("run %s: %w: %s", name, err, message)
		}
		return nil, fmt.Errorf("run %s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

type pulumiStackOutputReader struct {
	runner    commandRunner
	env       []string
	pulumiDir string
}

func (reader pulumiStackOutputReader) ReadSecretIDs(
	ctx context.Context,
	stack string,
) (secretIDsByHost, error) {
	args := []string{
		"stack", "output", "secretIdsByHost",
		"--json", "--show-secrets",
		"--stack", stack,
		"-C", reader.pulumiDir,
	}
	output, err := reader.runner.Run(ctx, "pulumi", args, nil, reader.env)
	if err != nil {
		return nil, fmt.Errorf("read Pulumi secret outputs: %w", err)
	}
	var secretIDs secretIDsByHost
	if err := json.Unmarshal(output, &secretIDs); err != nil {
		return nil, fmt.Errorf("decode Pulumi secret outputs: %w", err)
	}
	if secretIDs == nil {
		return nil, errors.New("pulumi output is missing secretIdsByHost")
	}
	return secretIDs, nil
}

type secretEncryptor interface {
	Encrypt(ctx context.Context, plaintext io.Reader, recipients string, env []string) ([]byte, error)
}

type sopsEncryptor struct {
	runner commandRunner
}

func (encryptor sopsEncryptor) Encrypt(
	ctx context.Context,
	plaintext io.Reader,
	recipients string,
	env []string,
) ([]byte, error) {
	args := []string{
		"--encrypt",
		"--input-type", "yaml",
		"--output-type", "yaml",
		"--age", recipients,
		"/dev/stdin",
	}
	sealed, err := encryptor.runner.Run(ctx, "sops", args, plaintext, env)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret with sops: %w", err)
	}
	return sealed, nil
}

type options struct {
	stack      string
	host       string
	out        string
	pulumiDir  string
	generation int
}

type sopsConfig struct {
	CreationRules []sopsCreationRule `yaml:"creation_rules"`
}

type sopsCreationRule struct {
	PathRegex string `yaml:"path_regex"`
	Age       string `yaml:"age"`
}

func parseArgs(args []string) (options, error) {
	parsed := options{}
	flags := flag.NewFlagSet("host-bao-seal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&parsed.stack, "stack", "", "Pulumi stack in org/stack form")
	flags.StringVar(&parsed.host, "host", "", "host whose secret_id to seal")
	flags.StringVar(&parsed.out, "out", "", "encrypted output path")
	flags.StringVar(&parsed.pulumiDir, "pulumi-dir", "", "Pulumi project directory holding the secrets stack, relative to the repository root")
	flags.IntVar(&parsed.generation, "generation", 0, "newest secret_id generation to seal")
	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(parsed.stack) == "" {
		return options{}, errors.New("--stack is required")
	}
	if strings.TrimSpace(parsed.host) == "" {
		return options{}, errors.New("--host is required")
	}
	if strings.TrimSpace(parsed.out) == "" {
		return options{}, errors.New("--out is required")
	}
	if strings.TrimSpace(parsed.pulumiDir) == "" {
		return options{}, errors.New("--pulumi-dir is required")
	}
	if parsed.generation <= 0 {
		return options{}, errors.New("--generation must be a positive integer")
	}
	return parsed, nil
}

func recipientsForOutput(out string) (string, error) {
	absOut, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	configPath, configDir, err := findSopsConfig(filepath.Dir(absOut))
	if err != nil {
		return "", err
	}
	contents, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		return "", fmt.Errorf("read SOPS config %q: %w", configPath, err)
	}
	var config sopsConfig
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return "", fmt.Errorf("decode SOPS config %q: %w", configPath, err)
	}
	relativePath, err := filepath.Rel(configDir, absOut)
	if err != nil {
		return "", fmt.Errorf("resolve output path relative to SOPS config: %w", err)
	}
	relativePath = filepath.ToSlash(relativePath)
	for _, rule := range config.CreationRules {
		pattern, err := regexp.Compile(rule.PathRegex)
		if err != nil {
			return "", fmt.Errorf("invalid SOPS path_regex %q in %q: %w", rule.PathRegex, configPath, err)
		}
		if !pattern.MatchString(relativePath) {
			continue
		}
		if strings.TrimSpace(rule.Age) == "" {
			return "", fmt.Errorf("SOPS creation rule %q has no age recipients", rule.PathRegex)
		}
		return rule.Age, nil
	}
	return "", fmt.Errorf("no SOPS creation rule in %q matches output %q", configPath, relativePath)
}

func findSopsConfig(start string) (string, string, error) {
	directory := start
	for {
		configPath := filepath.Join(directory, ".sops.yaml")
		if _, err := os.Stat(configPath); err == nil {
			return configPath, directory, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect SOPS config %q: %w", configPath, err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", "", fmt.Errorf("no .sops.yaml found above output directory %q", start)
		}
		directory = parent
	}
}

func runSeal(
	ctx context.Context,
	parsed options,
	reader stackOutputReader,
	encryptor secretEncryptor,
	env []string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	wantBase := parsed.host + "-bao.yaml"
	if filepath.Base(parsed.out) != wantBase {
		return fmt.Errorf("--out basename must be %q for host %q", wantBase, parsed.host)
	}
	recipients, err := recipientsForOutput(parsed.out)
	if err != nil {
		return err
	}
	idsByHost, err := reader.ReadSecretIDs(ctx, parsed.stack)
	if err != nil {
		return err
	}
	secretID, generation, err := newestSecretID(idsByHost, parsed.host)
	if err != nil {
		return err
	}
	if generation != parsed.generation {
		return fmt.Errorf(
			"requested generation %d is not newest for host %q (newest is %d)",
			parsed.generation, parsed.host, generation,
		)
	}

	//nolint:gosec // G117: The plaintext is marshaled only for SOPS encryption.
	plaintext, err := yaml.Marshal(struct {
		SecretID string `yaml:"secret-id"`
	}{SecretID: secretID})
	if err != nil {
		return fmt.Errorf("encode secret_id for sops: %w", err)
	}
	sealed, err := encryptor.Encrypt(ctx, bytes.NewReader(plaintext), recipients, envWithoutSecret(env, secretID))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(parsed.out, sealed); err != nil {
		return fmt.Errorf("write sealed secret file: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "generation=%d\n", generation); err != nil {
		return fmt.Errorf("write sealed generation: %w", err)
	}
	hash := sha256.Sum256([]byte(secretID))
	if _, err := fmt.Fprintf(stdout, "%x\n", hash); err != nil {
		return fmt.Errorf("write plaintext SHA-256: %w", err)
	}
	return nil
}

// writeFileAtomic replaces path through a same-directory temp file, so a failed write never truncates it.
func writeFileAtomic(path string, data []byte) (err error) {
	// CreateTemp makes the file 0600, the sealed-file mode.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(temp.Name()))
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write temp file: %w", err), temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync temp file: %w", err), temp.Close())
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

func newestSecretID(idsByHost secretIDsByHost, host string) (string, int, error) {
	generations, found := idsByHost[host]
	if !found {
		return "", 0, fmt.Errorf("unknown host %q in secretIdsByHost", host)
	}
	if len(generations) == 0 {
		return "", 0, fmt.Errorf("host %q has no secret_id generations", host)
	}
	var newest int
	var newestID string
	first := true
	for generation, secretID := range generations {
		if first || generation > newest {
			newest = generation
			newestID = secretID
			first = false
		}
	}
	if newestID == "" {
		return "", newest, fmt.Errorf("host %q generation %d has an empty secret_id", host, newest)
	}
	return newestID, newest, nil
}

func envWithoutSecret(env []string, secret string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.Contains(entry, secret) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func execute(ctx context.Context, args []string, runner commandRunner, env []string, stdout, stderr io.Writer) error {
	parsed, err := parseArgs(args)
	if err != nil {
		return err
	}
	reader := pulumiStackOutputReader{runner: runner, env: env, pulumiDir: parsed.pulumiDir}
	return runSeal(ctx, parsed, reader, sopsEncryptor{runner: runner}, env, stdout, stderr)
}

func main() {
	if err := execute(
		context.Background(), os.Args[1:], execCommandRunner{}, os.Environ(), os.Stdout, os.Stderr,
	); err != nil {
		fmt.Fprintf(os.Stderr, "host-bao-seal: %v\n", err)
		os.Exit(1)
	}
}
