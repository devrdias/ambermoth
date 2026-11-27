# Instructions for coding agents

For any AI agent working in this repo (Claude Code, Codex, Cursor, Copilot and others). Read
[CONTRIBUTING.md](CONTRIBUTING.md) for commits and releases and [docs/go-rules.md](docs/go-rules.md) for code.
What follows is what agents get wrong most.

## Commits

- Conventional Commits, as in CONTRIBUTING: the type decides the release. Scopes in use: `proxy`, `ledger`, `pack`,
  `verify`, `tsa`, `cli`, `report`, `release`, `deps`, `docs`.
- A body that says why. No `Co-Authored-By`, "Generated with" or other attribution lines.
- Run `make hooks` once per clone. Never bypass the `commit-msg` hook with `--no-verify`; fix the message.
- Use the git identity already configured. Don't change the author or committer.

## Code

- The core module uses the standard library only; a new dependency needs an ADR in `docs/adr/`.
- Run `make check`, `make rules` and `make crap` before committing.
- Never add code that edits or deletes existing ledger entries. Corrections are new entries.

## Writing

Comments, docs, commit messages and PR descriptions should read like a person wrote them by hand. Not like a model.

- Short and plain. Say a thing once. Prefer a sentence to a bullet list, and a list to a table.
- Comments say why, and only when the code can't. Don't narrate what the next line does.
- Doc comments on exported names start with the name and fit on one line where they can.
- No filler or sales words: "robust", "seamless", "comprehensive", "leverage", "ensure", "powerful",
  "simply", "note that", "it's worth noting", "this allows you to".
- No emoji, no bold lead-ins on every bullet, no headings for a two-line section.
- Facts don't change when wording does: commands, flags, numbers, limits, error codes, file names and
  formats stay exactly as they are.

This is about prose. It never lowers code quality, and docs/go-rules.md still decides how code is written.
