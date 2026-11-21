# garage-bucket-reconcile

Two commands for a Garage object store:

- `garage-bucket-reconcile` converges one bucket: it creates the bucket if it is missing, sets its size quota, and grants one access key read and write access without owner rights.
- `garage-admin-token-mint` mints a scoped Garage admin token and publishes it into a Kubernetes Secret. It replaces the token before it expires.

Both commands are built from this module and released as binaries. Hosts run the release, not a local build.

## Required environment

Every deployment value comes from the environment. None has a default. A missing or blank value exits 1 and names the variable.

### garage-bucket-reconcile

| Variable | Meaning |
| --- | --- |
| `GARAGE_ADMIN_URL` | Base URL of the Garage admin API. Trailing slashes are removed. |
| `GARAGE_ADMIN_TOKEN` | Admin token that is allowed to read and change the bucket. |
| `S3_ACCESS_KEY_ID` | S3 access key that receives read and write access. |
| `BUCKET_NAME` | Global alias of the bucket to create or converge. |
| `BUCKET_MAX_SIZE_BYTES` | Bucket size quota in bytes. A non-negative safe integer. |

### garage-admin-token-mint

| Variable | Meaning |
| --- | --- |
| `GARAGE_ADMIN_URL` | Base URL of the Garage admin API. Trailing slashes are removed. |
| `GARAGE_MASTER_TOKEN` | Master admin token, used only to mint and delete scoped tokens. |
| `POD_NAMESPACE` | Namespace of the Secret that receives the token. |
| `SECRET_NAME` | Name of the Secret that receives the token. |
| `TOKEN_NAME` | Name given to the minted admin token. |
| `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT` | In-cluster API server address. The cluster injects both. |

The mint command reads the service account token and CA from the standard in-cluster mount.

## Behaviour

- Each command exits 0 on success and 1 on any failure.
- The admin token is never written to stdout, stderr, or argv. Error text replaces it with `[redacted]`.
- HTTP redirects are refused on every request that carries a token. A 3xx response is a failure, so a redirect cannot resend the token to another host.
- The mint command keeps its token for 30 days and republishes it once seven days or less remain.

## Run and test

```sh
GOWORK=off go run ./cmd/garage-bucket-reconcile
GOWORK=off go run ./cmd/garage-admin-token-mint
go test -race ./...
```
