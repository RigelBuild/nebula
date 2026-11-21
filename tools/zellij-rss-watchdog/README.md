# zellij-rss-watchdog

Captures diagnostics when a zellij server exceeds its rolling RSS baseline. It samples one server, identified by the user that owns it and its session name. `--journal-unit` names the user systemd unit whose journal goes into each capture. All three are required flags with no default.

## Usage

```bash
zellij-rss-watchdog --user <account> --session <session-name> --journal-unit <unit>
```

Set `XDG_RUNTIME_DIR` when the runtime directory is not `/run/user/<uid>`. Set `WAVE_SUPERVISOR_RELAUNCH` to an absolute path of the relaunch tool; without it, restart requests are logged and skipped.

Captures are written under `/var/lib/zellij-rss-watchdog`, the newest three are kept, directories are mode 0750 and files 0640. Raw memory dumps (`mapping-*.raw`) stay 0600 until they are reduced and deleted. On each start, the tool deletes raw dumps left by a killed capture and re-applies these modes to older captures.

When `VmRSS + VmSwap` stays above 10 GiB for 10 minutes on one server identity, the watchdog requests a relaunch as the configured user. It retries every 30 minutes while usage stays high.

Each capture includes bounded recent zellij logs, server smaps and status, the server's `ss -xp` rows and peer process details, reduced strings from up to four large anonymous mappings, the user journal for the `--journal-unit` unit, the zellij client list for the session, sampled RSS, and `errors.txt` when a step fails.

- **Memory:** the four largest anonymous read-write mappings of at least 64 MiB, at most 512 MiB each. Each is copied read-only from `/proc/<pid>/mem` in 4 MiB chunks after a starttime check, reduced, then deleted.
- **Logs:** read from `/tmp/zellij-<uid>/zellij-log` with `O_NOFOLLOW` on every path component; only regular files are read.
- **User commands:** run as the configured user through `runuser` under `timeout`, because the watchdog holds no `CAP_KILL`.
- **Timeouts:** a timed-out command's process group gets one SIGKILL while its leader is unreaped, and the command settles within a second even if an escaped process holds its output.

The live proc-memory, file-descriptor log, `setsid`, and `/proc`-based process-group death tests run only on Linux.
