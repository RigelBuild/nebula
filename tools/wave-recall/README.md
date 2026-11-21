# wave-recall

`wave-recall` runs one Hindsight recall query against one memory bank. The
dreamer calls it once per bank and query.

## Usage

```sh
HINDSIGHT_URL=<base URL> HINDSIGHT_API_KEY=<key> \
  wave-recall --bank example-bank --query "<text>" --max-tokens 2000
```

Flags:

- `--bank`: Hindsight bank id. Required.
- `--query`: recall query text. Required.
- `--max-tokens`: token budget for the response. Required, must be positive.

Environment:

- `HINDSIGHT_URL`: base URL of the Hindsight API. Required. Must be an absolute `http` or `https` URL with no query or fragment.
- `HINDSIGHT_API_KEY`: sent as `Authorization: Bearer <key>` when set.

## Output

One line per result: the memory id, a tab, then the memory text. A memory id
must match `^[A-Za-z0-9_-]+$`; a result with any other id fails the run. A
backslash prints as `\\`, a newline as `\n`, and a carriage return as `\r`.

## Exit codes

- `0`: the request succeeded. Every result is printed.
- `1`: the request failed, returned a non-2xx status, or returned a malformed
  response. The one-line error on stderr names the bank. Nothing is printed on
  stdout.
- `2`: bad command line.

## API

`POST /v1/default/banks/{bank_id}/memories/recall`. The body is
`{"query": ..., "max_tokens": ...}`. The response is
`{"results": [{"id": ..., "text": ...}]}`.

Sources, from [vectorize-io/hindsight](https://github.com/vectorize-io/hindsight)
at API version 1.2.1, read at commit `289fb421` (the sha the client model file
resolved at). A later change to these files does not change this citation:

- `hindsight-clients/go/api_memory.go`, `RecallMemoriesExecute`: the path, method and headers.
- `hindsight-clients/go/model_recall_request.go`: `query` and `max_tokens`.
- `hindsight-clients/go/model_recall_response.go` and
  `model_recall_result.go`: `results`, `id` and `text`.

The OMP client, `packages/coding-agent/src/hindsight/client.ts`, `recall`, sends
the same path, body and `Bearer` header.

## Tests

```sh
go test ./tools/wave-recall/...
```
