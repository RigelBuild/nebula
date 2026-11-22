# rbuild-workspace-prune

Prune idle workspace trees. Repeat `--root` for each root. A bare root selects the existing seat/workspace layout. A `DIR=GLOB` root selects directories at the exact level matched by the glob, relative to `DIR`.

```sh
rbuild-workspace-prune \
  --root /home/rbuild/rbuild \
  --root '/home/rbuild/agents=workspaces/*/*' \
  --root '/home/rbuild/agents=rigel/clones/*' \
  --root '/home/rbuild/agents=rigel/fix-6766b-run/*' \
  --root '/home/rbuild/rjc-hook=RigelBuild/*/w[0-9]*'
```

The exact-level patterns leave bare `*.git` caches and the rjc-hook `tools`, `slots`, and `queue` directories untouched. Use `--dry-run` to inspect candidates. `--max-idle` defaults to 24 hours.

Each candidate must have no recently modified entry anywhere in its tree. The tool checks process working directories and open files, stages each candidate by renaming it, and scans processes again before deletion. It also rescans staged leftovers. A held slot lock does not block pruning; a run that has acquired a slot but has not yet opened or entered an idle tree is not visible to the process scan.

`--home` selects the fleet home layout instead of `--root`. A home tree whose idle walk exceeds its depth or time bound is kept.
