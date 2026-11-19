# rumdl-base-config

Shared rumdl base policy. The flake exports it as
`packages.<system>.rumdl-base-config`, a store path that holds
`rumdl/base.toml`. It is not published to npm.

A consumer sets `RUMDL_BASE_CONFIG` to that file, then extends it in its
`.rumdl.toml`:

```toml
extends = "$RUMDL_BASE_CONFIG"
```

`base.toml` is the policy. `test/rumdl-extends.test.ts` checks its behaviour
with the `rumdl` on PATH: a consumer that extends it passes a long line and
fails an indented code block.
