# host-bao-seal

Seal the newest host AppRole `secret_id` from the Pulumi secrets stack into an age-encrypted SOPS YAML file. The plaintext is passed to SOPS over stdin and never placed in a command argument or child environment.

## Usage

```sh
GOWORK=off go run ./tools/host-bao-seal --stack example-org/prod --pulumi-dir infra/pulumi/secrets --host example-host --out secrets/example-host-bao.yaml --generation 2
```

Run from the repository root. `--pulumi-dir` names the Pulumi project that holds the secrets stack and is required. `--out` must be named `<host>-bao.yaml`; the tool loads the nearest `.sops.yaml` and uses the age recipients from its matching creation rule. `--generation` is required and must match the newest generation returned by Pulumi. On success, stdout contains the SHA-256 of the secret ID for the rotation gate, and stderr prints `generation=<n>`. Pulumi, SOPS, and age must be installed and configured. The SOPS-age round-trip test requires `sops` and `age-keygen`; set `HOST_BAO_SEAL_SKIP_SOPS_TEST=1` only to explicitly opt out.
