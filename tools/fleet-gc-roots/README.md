# fleet-gc-roots

Lists Nix GC roots that sit in idle agent workspaces. It is a dry run: it reads
`nix-store --gc --print-roots`, decides which roots belong to idle workspaces, and
writes a report. It never deletes a root and never runs garbage collection.

## Usage

```sh
FLEET_GC_ROOTS_WORKSPACES=/path/to/workspaces \
FLEET_GC_ROOTS_OUTPUT=/path/to/state/candidates.txt \
  fleet-gc-roots
```

Run it on a schedule as the user that owns the workspace tree. It writes only to
`FLEET_GC_ROOTS_OUTPUT` and its `.tmp` sibling.

## Rules

- A workspace is `<workspaces>/<seat>/<ws>`. A root is judged by the workspace it sits in: it is
  idle when the newest mtime of the workspace directory and of each `.jj/working_copy` from the
  workspace down to the root's parent is more than seven days old. A nested jj workspace, such
  as `team/agent-3/repo`, counts its own `.jj/working_copy`, so an active one keeps its
  roots out of the report.
- A workspace directly under `<workspaces>`, such as `<workspaces>/ws-9027`, has no seat
  folder, so its own roots are not scanned.
- A busy workspace is skipped. A process whose cwd is inside it marks it busy.
- A workspace deleted or replaced by a file during the run is skipped and counted as vanished. Its roots are not candidates.
- A root is a candidate when it lies inside an idle workspace and either a path part
  below `<seat>/<ws>` is `.devenv` or `.direnv`, or its base name is `result` or
  starts with `result-`.

## Output

The report goes to stdout and to `FLEET_GC_ROOTS_OUTPUT`. The first
line gives the count of candidate roots and workspaces. The rest is one candidate
root per line.

## Inputs

| Input | Source |
| --- | --- |
| Workspace tree | `FLEET_GC_ROOTS_WORKSPACES`. Required, no default. |
| Report file | `FLEET_GC_ROOTS_OUTPUT`. Required, no default. |
| Process cwds | `/proc`, fixed in `main.go`. |
| Nix roots | `nix-store --gc --print-roots`, fixed argv, no shell. |

The tool takes no arguments. Any argument exits with status 2. A missing required
variable also exits with status 2.
