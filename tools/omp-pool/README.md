# omp-pool

Stage OMP OAuth logins locally, upload them to the primary k3s broker on the pool host, remove a broker credential, and reset cached account usage on that broker.

```text
omp-pool login <primary> [provider]
omp-pool status
omp-pool push <primary> [--dry-run] [--yes] [--host H]
omp-pool remove <primary> <account> [--dry-run] [--yes] [--host H]
omp-pool reset-usage <primary> <account> [--dry-run] [--yes] [--host H]
```

## Required environment

Broker commands read every deployment input from the environment. None has a
default. A missing value fails the command with usage exit code 2 and names
each missing variable.

| Variable | Meaning |
| --- | --- |
| `OMP_POOL_HOST` | SSH destination for the pool host, for example `user@host`. `--host H` overrides it for one run. |
| `OMP_POOL_KUBECONFIG` | Path to the kubeconfig on the pool host that `kubectl` uses. |
| `OMP_POOL_NAMESPACE` | Kubernetes namespace that holds the broker pod and Secret. |
| `OMP_POOL_BROKER_POD` | Name of the broker pod to port-forward to. |
| `OMP_POOL_BROKER_SECRET` | Name of the Kubernetes Secret that holds the broker bearer token. |

`login` and `status` need none of these. Logins stage under
`$OMP_POOL_STAGE_DIR` if set, otherwise `~/.local/share/omp-pool/primary`.
`status` checks that staging directory without network access.

## Behavior

Closing an SSH forward sends EOF to the remote command, which stops and reaps the remote `kubectl port-forward`. It sends SIGTERM first and escalates to SIGKILL after two seconds if the child has not exited.

Push previews the primary broker and uploads OAuth credentials only. Review the printed plan; use `--dry-run` to stop before confirmation or `--yes` to skip confirmation. A successful upload deletes the staging directory.

`reset-usage` resolves an email or provider account ID on the primary broker, then clears that credential's rate-limit blocks and invalidates the provider's cached usage report there. Usage invalidation re-polls every credential for that provider on the primary broker. Use `--dry-run` to preview without mutation, or `--yes` to skip confirmation.

`remove` resolves an email or provider account ID on the primary broker and soft-disables that credential. The broker retains a disabled tombstone, but active snapshots omit it. The command reads a fresh snapshot and reports success only when the credential ID is absent. Use `--dry-run` to preview without mutation, or `--yes` to skip confirmation.

## Example

```sh
export OMP_POOL_HOST=example-user@example-host
export OMP_POOL_KUBECONFIG=/etc/example/broker.kubeconfig
export OMP_POOL_NAMESPACE=example-ns
export OMP_POOL_BROKER_POD=example-pod-0
export OMP_POOL_BROKER_SECRET=example-secret

omp-pool push primary --dry-run
```

After a push, add each account to your roster with `pool = "primary"`, then
check the broker on the pool host. The command prints the exact `ssh` and
`kubectl` line for your environment.
