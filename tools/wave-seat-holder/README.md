# wave-seat-holder

A Go CLI that runs each agent process in its own tmux server, managed by a
transient systemd user unit. It reads `seat.conf` beside its binary. Zellij
panes attach to the tmux session and can be restarted without stopping the
agent.

```sh
wave-seat-holder start --name <name> -- <command> [args...]
wave-seat-holder attach --name <name> [--read-only]
wave-seat-holder interrupt --name <name>
wave-seat-holder list --json
```

`start` prints the holder and agent process IDs on success. It returns exit code
4 when the named unit or socket is already live. Seat sockets are stored under
`$WAVE_SEAT_SOCKET_DIR`, or `$XDG_RUNTIME_DIR/cotal-seats` by default. The unit
name prefix and slice can be changed with `WAVE_SEAT_UNIT_PREFIX` and
`WAVE_SEAT_SLICE`.
Socket paths may be at most 103 bytes on Darwin and 107 bytes on Linux.

A launcher that dies at once has the last 20 pane lines captured; up to 10 non-blank lines go to the holder's stderr, which the journal keeps.

The real-tmux teardown test uses `WAVE_SEAT_TEST_TMUX` as the tmux binary, or `tmux` on PATH, and skips when neither is set.
