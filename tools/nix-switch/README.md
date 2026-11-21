# nix-switch

On a Mac, `nix-switch` prompts for sudo, fetches the repository checkout, starts
an empty working copy on `main`, and runs `nh darwin switch`. It refuses to
sync if the working copy has changes or the local `main` bookmark is conflicted.

Use `nix-switch --local` to switch the current working copy without syncing.
Use `--repo PATH` to select another checkout. Pass nh arguments after `--`,
for example `nix-switch --config FILE --host NAME --local -- --verbose`.

## Config

Every deployment value comes from a CUE file. The tool has no default for any
of them. Pass the file with `--config`, and name one of its hosts with `--host`.
Both flags are required.

```cue
repo:  "repos/example/checkout"
flake: "example/nix"
hosts: ["example-host"]
```

- `repo`: the checkout. A relative path resolves against `$HOME`. `--repo` overrides it.
- `flake`: the directory inside the checkout that holds the darwin flake. It must be relative.
- `hosts`: the darwinConfigurations names that `--host` may select. At least one.

The file must satisfy `schema.cue`, which the binary embeds. It may not import packages.
