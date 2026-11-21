# service-urls

Generates the service links page and Glance bookmarks from a service URL
inventory, and checks that inventory against deployed endpoints and managed
repositories.

## Usage

```sh
service-urls --config /path/to/config.cue            # generate the links page and Glance config
service-urls --config /path/to/config.cue coverage   # check inventory coverage
service-urls --config /path/to/config.cue repo-inventory
```

`--config` is required on every command. It points at a CUE file that matches
[`config/schema.cue`](config/schema.cue). Every field
is required and the schema sets no defaults. Paths in the config are relative to
the workspace root.

The workspace root is the nearest ancestor of the working directory that holds
`groups.toml` under the config's `inventoryPath`.

## Config

| Field | Meaning |
| --- | --- |
| `internalHostSuffixes` | DNS suffixes reachable only on the tailnet. At least one. |
| `tailscaleDomain` | Domain that full tailnet hosts end in. |
| `dnsZone` | Zone appended to each internal DNS record name. |
| `inventoryPath` | Directory with `groups.toml` and the link tables. |
| `linksPagePath`, `glancePath` | Generated outputs. |
| `portalSource` | Source of the Glance portal row, kept off the bookmarks. |
| `tailnetPath`, `unionPath`, `dnsRecordsPath` | Inputs for endpoint discovery. |
| `managedReposPath`, `cloudflareHostsPath`, `workerDomainsPath` | Inputs for repository and host coverage. |
| `owners` | Source prefixes that each generator owns: `kubernetes`, `dns`, `cloudflare`, `workers`. |

## repo-inventory

Needs these environment variables:

- `REPO_INVENTORY_APP_PRIVATE_KEY`: GitHub App private key PEM.
- `REPO_INVENTORY_APP_CLIENT_ID`: GitHub App client ID.
- `REPO`: repository in `owner/name` form.

## Exit codes

- `0`: success, or the inventory is complete.
- `1`: inventory gaps.
- `2`: operational error.
