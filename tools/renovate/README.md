# Renovate preset

`preset.json5` is the shared Renovate preset for Rigel repositories. It extends
`config:recommended` and sets:

- `dependencyDashboard`: `true`, so Renovate keeps a dashboard issue of pending updates.
- `rebaseWhen`: `behind-base-branch`, so Renovate rebases a PR when its base branch moves ahead.
- `minimumReleaseAge`: `5 days`, a supply-chain cooldown. Renovate waits five days after a release before proposing it.
- `internalChecksFilter`: `strict`, which holds a PR until a version clears the cooldown.

## Use

Extend the preset from a repository's Renovate config:

```json5
{
  $schema: "https://docs.renovatebot.com/renovate-schema.json",
  extends: ["github>RigelBuild/nebula//tools/renovate/preset.json5"],
}
```

## Test

`moon run renovate:test` validates the preset with `renovate-config-validator`.
The task takes node from the nix flake's pinned nixpkgs, because the devshell has no node.
