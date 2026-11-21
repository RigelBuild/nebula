# linear-acceptance

Refuses `linear issue create` unless the description has a `## Acceptance criteria` section with at least one `- [ ]` item. When the check passes, it runs the real `linear` CLI with the same arguments. Every other subcommand passes straight through.

## Usage

```bash
GOWORK=off go run ./tools/linear-acceptance <real-linear> issue create -t "Title" --description-file issue.md
```

The check reads the description from `--description-file` or from `-d`/`--description`. A refusal exits 2 and prints one line to stderr that names what is missing.

To create an issue without the section, pass `--no-acceptance-criteria <reason>`. The reason must be non-empty. The tool strips the flag and prints the reason to stderr before it runs `linear`.

The packaged binary can wrap the real `linear` CLI in any host's shell, so agents and humans get the same check everywhere.

## Credentials

None.
