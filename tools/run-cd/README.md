# run-cd

Runs one CD target. It gates on the Woodpecker pipeline history, holds a
Kubernetes Lease while the deploy runs when `--lease` is given, and exits with
the deploy's code.

```sh
run-cd <target> [--lease <name>] --members <workflow,...> \
  --in-cluster-url <url> --pulumi-root <dir> --pulumi-tiers <tier,...> \
  -- <command...>
run-cd --heartbeat
```

- `--in-cluster-url`: the Woodpecker API address reachable from inside the cluster.
- `--pulumi-root`, `--pulumi-tiers`: each tier is a directory under the root;
  its children that hold a `Pulumi.yaml` are Pulumi stacks.

All three are required. There are no defaults, because the right values depend
on the deployment. The gate logic lives in `tools/go-lib/cdgate`.
