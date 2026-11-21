# cotal-presence-probe

A read-only Cotal observer. It watches seat presence in one Cotal space and serves Prometheus metrics on loopback. A seat whose provider refused its session history holds in `waiting` until restarted. The `cotal_seat_history_refused_age_seconds` gauge dates that hold, so an alert can fire within minutes.

## Usage

```sh
WAVE_SEATS_FILE=/path/to/wave-seats.json \
RUNTIME_DIRECTORY=/path/to/runtime \
cotal-presence-probe
```

`RUNTIME_DIRECTORY` must hold `observer.creds`, the observer's NATS credentials file, before the probe starts. The probe serves `/metrics` and `/healthz` on `127.0.0.1:9249` by default.

## Configuration

| Variable | Required | Default |
| --- | --- | --- |
| `WAVE_SEATS_FILE` | yes | none |
| `RUNTIME_DIRECTORY` | yes | none |
| `COTAL_PRESENCE_PROBE_PORT` | no | `9249` |
| `COTAL_SPACE` | no | `main` |
| `COTAL_SERVER` | no | `nats://127.0.0.1:4222` |

A required value with no default makes the probe exit non-zero at start.

The seat list is read once at start. A missing or unparseable seats file exits non-zero. Restart the probe to refresh the list.

## Metrics

| Metric | Labels | Meaning |
| --- | --- | --- |
| `cotal_presence_probe_up` | none | 1 when connected and the presence view is current. |
| `cotal_seats` | `status` | Roster count per status; all four statuses always present. |
| `cotal_seats_history_refused` | none | Count of wedged agent seats. |
| `cotal_seat_history_refused_age_seconds` | `seat` | Age of the 20 oldest wedged seats. `seat` is a name from the seats file or `other`. |

A seat is wedged when it is an agent in `waiting` with an activity starting `provider refused this session's history` and a set `statusSince`.

## Development

Bun 1.4.0 or newer.

```sh
bun install
bun test
bun run typecheck
bun run lint
```

## License

MIT OR Apache-2.0
