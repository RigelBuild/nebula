# wave-ledger

Joins the wave usage ledger with GitHub and Linear to report daily outcomes,
per-group cost, and per-driver prompt attribution. It reads ledger files and
never writes to the ledger except for the outcomes file.

## Commands

```sh
wave-ledger report   --dir <ledger> --from YYYY-MM-DD --to YYYY-MM-DD --by <keys> [--json] [--prices <file>]
wave-ledger drivers  --dir <ledger> --from YYYY-MM-DD --to YYYY-MM-DD [--json]
wave-ledger outcomes --dir <ledger> --repos <owner/repo,...> --issue-prefix <PREFIX> [--date YYYY-MM-DD] [--prices <file>]
```

## Required inputs

Every deployment value is a flag with no default. The tool refuses to run
without it.

| Input | Flag | Used by | Rule |
| --- | --- | --- | --- |
| Ledger directory | `--dir` | all commands | Required. The directory holds `YYYY-MM-DD.jsonl` usage rows, `content/`, and `outcomes/`. |
| Repositories | `--repos` | `outcomes` | Required. Comma-separated `owner/repo` list. Repositories are searched in the given order. |
| Issue key prefix | `--issue-prefix` | `outcomes` | Required. An uppercase key such as `PRJ`. Issue IDs match `PREFIX-N`, and bookmark names match `prefix-N-`. |

## Optional inputs

| Input | Flag | Default |
| --- | --- | --- |
| Price table | `--prices` | An embedded table with no entries. Costs are then taken only from the catalog cost on each row. |
| Outcome date | `--date` | Today, UTC. |

## Environment

`outcomes` needs a Linear credential. It reads a key file named by
`LINEAR_API_KEY_FILE` first, then the `LINEAR_API_KEY` variable.

## Outputs

- `report` writes text or JSON to stdout.
- `drivers` writes text or JSON to stdout. It never prints prompt or message
  text, only byte counts and shares.
- `outcomes` writes `<dir>/outcomes/YYYY-MM-DD.jsonl` through a temporary file
  and a rename. A failed run leaves no partial file.

## Example

```sh
LINEAR_API_KEY_FILE=/path/to/linear-key \
  wave-ledger outcomes --dir /path/to/ledger \
    --repos example-org/app,example-org/tools --issue-prefix EX
```
