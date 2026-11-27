# Contributing

- Standard library only. A new dependency needs an ADR in `docs/adr/`, because reviewers must be able to audit the verifier.
- Any change to hashing, canonical form, entry fields or the pack layout is a format change. Update `docs/SPEC.md` in the same PR, and bump the format version when compatibility breaks.
- Never add a feature that edits or deletes existing ledger entries. Corrections are new entries.
- Go code follows [docs/go-rules.md](docs/go-rules.md), including [ub-mode](https://github.com/unclebob/ub-mode). The skill is in `.claude/skills/ub-mode/` (MIT, © Robert C. Martin).
- Run `make check` before sending a PR. The TSA tests need `openssl` on PATH.

## Commit messages and releases

Releases are automatic. Every merge to `main` runs [svu](https://github.com/caarlos0/svu), which reads the commit messages since the last tag and picks the next version. If there is one, CI tags it and [GoReleaser](https://goreleaser.com) builds binaries for Linux, macOS and Windows, writes the release notes from the commits and publishes the release. Both are Go tools, so the release needs no Node. Nothing is committed back to the repository; the notes live on the [releases page](https://github.com/devrdias/ambermoth/releases).

That means commit messages must follow [Conventional Commits](https://www.conventionalcommits.org): `type(optional scope): summary`.

| Commit | Release |
|---|---|
| `fix: ...` | patch (0.2.0 → 0.2.1) |
| `feat: ...` | minor (0.2.0 → 0.3.0) |
| `feat!: ...`, or a `BREAKING CHANGE:` footer | major (0.2.0 → 1.0.0) |
| `perf`, `refactor`, `build`, `docs`, `test`, `ci`, `chore`, `style` | no release on its own |

A change users should get, such as a performance improvement or a dependency security update, ships as `fix:` (or rides along with the next release). The release notes still list `perf`, `refactor` and `build` commits under their own headings.

A change to the ledger or pack format that breaks compatibility is a breaking change: mark it with `!` and explain it in a `BREAKING CHANGE:` footer.

Run `make hooks` once after cloning. It installs a `commit-msg` hook (plain shell, no dependencies) that rejects any message not in this format before the commit is made. CI runs the same script on pull requests, so if it passes locally it passes there.

Pull requests are checked by the `commits` workflow. With a squash merge the PR title becomes the commit on `main`, so the title must follow the format. With a merge commit or a rebase, every commit must.
