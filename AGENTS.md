# Working in this repository

troupe is a Go CLI that manages Hermes agents, each in its own Docker Sandbox. The [README](README.md) is the end-user reference (commands, security model, build); [docs/development.md](docs/development.md) covers the dev build (`-tags dev`: the maintainer commands and the kits root), which is never released.

## Hard boundaries

- Local Git on `main`: never add a remote or push without the owner's explicit request. Inspect `git status --short` before editing, preserve unrelated changes, never clean or reset destructively without approval.
- Never commit automatically. Report verified scope and evidence, then ask the owner to approve. Before asking, audit the uncommitted diff once with an independent reviewer against the README's [security model](README.md#security-model).
- This repository is meant to be published: nothing private goes in it, no real agent, person, account, channel or machine names, identifiers or credentials. Examples use the fictional kit in [`examples/kit`](examples/kit). Before asking to commit, run the owner's private scan, kept outside this repository: `python3 -m unittest discover -s ~/hermes-agents/tests -p test_troupe_private.py`.
- Never invoke `sudo`, even as a probe. Hand the owner one reviewable command.
- Before **any** `sbx` command, run the honest sandboxd probe (`troupe guard probe`).

## Working style

- Operator commands print only `[HH:MM:SS] Agent step: status. Message.` lines; `internal/progress` holds the grammar and its tests.
- A command that needs sudo asks for it first, before any other work or prompt, and keeps it fresh until it ends: a password prompt never comes midway. The only prompts allowed before it are confirmations (a destructive command's, or the resource budget's), so a declined one asks for nothing.
- Replies to the owner: no dash punctuation or hyphenated phrasing (use commas, colons, periods, parentheses; keep exact code and identifiers). Times in the owner's time zone. No duration estimates for plans or work.
- Tests: `go vet ./... && go test ./...`, the same with `-tags dev`, and `python3 -m unittest discover -s guest/tests`.
- Maintainer-only commands, the kits root, and the packages and in-VM jobs only they use are dev build only: a `//go:build dev` file, or `embed_dev.go` for a job. The release binary carries none of them; code a release runs (such as `storage.py`, which `fresh` imports) ships in both builds.
