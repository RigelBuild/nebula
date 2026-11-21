# wave-host

`wave-host status --json [--state path]` reads local host state. The default
state path is `~/agents/run/wave-host/state.json`. A missing file reports stopped;
unreadable or invalid state reports blocked. The state store writes mode 0600
files in a mode 0700 directory, using sync and atomic rename.

The platform seams identify a process by PID and start token, list children,
lock files without an external command, and enforce seat socket path limits.
On Darwin, identity uses `sysctl kern.proc.pid` and child enumeration uses
`sysctl kern.proc.all`. The start token is `p_starttime` as integer microseconds
(seconds × 1000000 + microseconds), so the Cotal holder parser reads it as digits.
This does not use cgo. Start and stop retain running records
unless both recorded PIDs have changed identity or the kernel reports them
absent via `kill(pid, 0)`.

On Darwin, `wave-host seat-start --name NAME --socket PATH --conf FILE --cwd DIR -- COMMAND...`
starts a foreground tmux launchd job and prints the holder and agent PIDs.
`wave-host seat-stop --name NAME` removes that job. Both commands refuse an
orphan label; start cleanup runs `kill-server` only when the socket exists.
The per-name flock serializes label use; out-of-band `launchctl bootstrap` of
`build.rigel.wave-seat.*` labels is unsupported.

`wave-host process-identity --pid N` prints the start token of one live process
and exits 0. The Cotal holder runtime on Darwin reads it to compare recorded seat
identities. Exit 3 means the process is absent. Exit 1 means it is unreadable, so
an unreadable identity is never reported as gone. Exit 2 is a usage error.

`wave-host start [--state path] -- COMMAND...` runs the host manager in the
foreground and records `starting`, then `ready` with its PID and start token.
A clean exit after a stop is `stopped`; any other exit is `blocked` with a reason.
`wave-host stop [--state path]` signals the recorded manager, waits for it to
exit, and records `stopped` for the current GUI login. Stop is sticky for that
login: start refuses until the next login. A failed stop records `blocked` and
keeps the manager PID and token, so a later start still refuses a live manager.
Both take `state.json.lock`, so start and stop never interleave a state write.
Start exits non-zero when refused, so a LaunchAgent must not use `KeepAlive`;
the agent runs once per login (`RunAtLoad`) and Cotal owns relaunch.

The Darwin login manager runs
`wave-host start -- cotal manager start --foreground --runtime holder --space main`
with `COTAL_HOLDER_COMMAND=wave-seat-holder` in its environment.

A login LaunchAgent runs that command with `RunAtLoad` and no `KeepAlive`, per
the rule above. Its output goes to the agent's own log file.
