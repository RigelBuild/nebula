feat(approval): show the reviewer the script a command runs and the session context it saw

The smart reviewer judges a plugin escalation from the command text alone when that text runs a
script by path, so it asks the human about scripts it cannot read.

- Send the reviewer the contents of each script a command runs by path (python[3], uv run, bash|sh,
  ./path), resolved against a `cd <dir> &&` prefix or the session cwd. Bounded to 16 KiB per file and
  three files, redacted, fenced and escaped. Only regular files whose realpath is under the home are
  read; a symlink out of the home is refused and noted.
- Record the rendered review context in approval-escalations.jsonl instead of the cwd-only block, so
  a record shows what the reviewer was sent. The record is redacted as before.
- Reviewer prompt: APPROVE a sudo run or browser fill the user already approved in this session; the
  current tab URL on a fill is not the fill target.

Co-authored-by: Matt Wilkinson <matt@rigel.build>
