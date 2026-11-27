# Go rules

How Go code in ambermoth is written. Rules are **must** unless marked *should*. They apply to
everything under `cmd/`, `internal/`, `tools/` and `examples/`, whoever writes the change, a
person or an agent. [CONTRIBUTING.md](../CONTRIBUTING.md) covers commits, releases and format
changes; this file covers the code.

`make rules` (the `tools/rulecheck` analyzer, run in CI) enforces the rules marked **[checked]**;
review covers the rest.

## 1. Stack

| Concern | Use |
|---|---|
| Go | 1.22+; `go.mod` at the repo root |
| Dependencies | standard library only ([ADR-0001](adr/0001-go-single-binary-stdlib-only.md)); `go.sum` stays empty |
| Storage | append-only JSONL ledger ([ADR-0002](adr/0002-jsonl-not-sqlite.md)) |
| Proxy | `net/http/httputil` pass-through ([ADR-0003](adr/0003-pass-through-not-gateway.md)) |
| Logging | `log/slog` |
| Tests | `testing` only (no testify, go-cmp or goleak; they would be dependencies) |
| Dev tools | run with `go run` at a pinned version (golangci-lint, gremlins); never imported by the module |

A new module dependency needs an ADR: what it does, why the standard library isn't enough,
maintenance status and license. Reviewers audit the verifier, and every dependency is something
they have to trust.

## 2. Layout

```
cmd/ambermoth/         # main(): wiring only
internal/cli/          # commands; takes the process (streams, env, clock, logger) through cli.Env
internal/ops/          # operations shared by commands: anchor, link, key events, reconcile
internal/pack/         # evidence pack build and verify
internal/proxy/        # recording reverse proxy and ledger writer
internal/report/       # summaries and advisory alerts
internal/ledger/       # entries, hash chain, store, verification
internal/canon/        # canonical JSON
internal/tsa/          # RFC 3161 requests and token verification
internal/trust/        # pinned TSA roots
tools/                 # dev tools (rulecheck, crap, brand); not shipped
examples/              # runnable examples
```

- Imports point down this list: `cli` → `ops`/`pack`/`proxy`/`report` → `ledger` → `canon`.
  `ledger` and `canon` never import a package above them; nothing imports `cli`.
- `cmd/ambermoth/main.go` holds only `main()`: build the signal context and `cli.Env`, call
  `cli.New(env).Run`, exit with its code. No commands, flags or logic; no other non-test file there.
- Process access (`os.Getenv`, `os.Stdin`/`Stdout`/`Stderr`, `os.Exit`) happens only in `main`.
  Commands get them through `cli.Env`. **[checked]** for `os.Getenv`/`LookupEnv`.
- No package named `util`, `common`, `helpers`, `base` or `misc`.

## 3. Go style

- `gofmt` on every file. MixedCaps, initialisms in one case (`ID`, `URL`, `TSA`, `seqID`).
- No stutter (`ledger.Entry`, not `ledger.LedgerEntry`); short receiver names, never `this`/`self`
  **[checked]**.
- Accept interfaces, return structs. Constructors return concrete types. Interfaces are
  declared by the package that uses them, sized to what it calls (1–3 methods *should*). Watch
  the typed-nil trap when storing a pointer in an interface.
- No package-level mutable state: no global clients, loggers, config, caches or `init()`.
  **[checked]** Allowed at package level: constants, sentinel errors, `var _ I = (*T)(nil)`,
  compiled regexps, `//go:embed`, parsed templates. A read-only table or a value Go cannot make
  `const` (an OID slice, the `-ldflags` version) carries a `//rules:readonly <reason>` comment.
- Closed sets of values are typed constants with every value named, not raw strings compared
  across the code.
- Early return; at most 3 levels of nested control statements; no `else` after `return`; no
  `goto` or labels. **[checked]**
- Never range over an inline composite literal; name the slice (or use a variadic helper) first.
  **[checked]**
- Comments: one-line doc on exported identifiers; otherwise only a *why* the code can't say. No
  issue, PR or review-finding numbers in code, comments or test names; they belong in the commit.
  **[checked]**

## 4. Errors, context, logging

- `ctx context.Context` is the first parameter of anything that does network I/O or may block
  (TSA requests, the proxy, pack builds). Never `context.Background()`/`TODO()` outside `main`
  and tests **[checked]**; never store a `ctx` in a struct.
- Wrap with `%w` and add context once per layer (`fmt.Errorf("anchor seq %d: %w", seq, err)`);
  check with `errors.Is`/`errors.As`. Handle an error or return it, never both.
- Errors a caller acts on are sentinels or typed errors in the package that returns them
  (`ledger.ErrNilClock`).
- Command errors reach the user once, from `cli.App.Run` (`error: …`, exit 1). Verification
  failures print their report and exit 1; bad command lines exit 2.
- `*slog.Logger` is injected through constructors; no `slog.Default()` or `log.*` outside `main`
  **[checked]**. Use the `…Context` variants and `snake_case` keys (`seq`, `provider`,
  `duration_ms`). Log a failure once, where it is handled.
- Never log or store secrets. API keys appear only as `sha256:` fingerprints; the master key
  never leaves `secret.key`; request and response bodies are hashed, never written out unless
  the user discloses them in a pack. Types that hold a key implement `slog.LogValuer` to mask it.

## 5. The ledger

- Entries are only ever appended, through `ledger.Store.Append`. Nothing edits or deletes an
  existing entry; a correction is a new entry (`amendment`).
- Hashes are computed over the canonical form (`internal/canon`). Anything that changes hashing,
  canonical form, entry fields or the pack layout is a format change: update `docs/SPEC.md` in
  the same PR, and when old ledgers would stop verifying, bump the entry `v` and keep `verify`
  accepting the old version.
- Time comes from the injected `ledger.Clock` (`Store.Now()`), never `time.Now()` outside `main`
  and tests **[checked]**. Timestamps are UTC (`ledger.FormatTime`).
- Writes are fsynced; one writer goroutine owns the ledger file in the proxy.
- Operations that may be retried (anchoring, key events, reconcile) must leave a valid ledger if
  interrupted or run twice: a retry appends a new entry, it never patches the old one.

## 6. The proxy

- It is a pass-through: bytes go to the provider and back unchanged; ambermoth only observes.
  It never rewrites bodies, models or routing.
- It fails open: if recording fails, the call still goes through and a `gap` entry records how
  many calls were missed. An invalid purpose header is recorded with a note, never rejected.
- Shutdown lets in-flight calls finish and be recorded before the ledger writer stops.
- It listens on loopback by default; anything else is an explicit user choice.

## 7. Concurrency

- Every goroutine has an owner and a visible exit; nothing starts goroutines in constructors
  (`New…`) or `init()` **[checked]**. Long-lived workers expose `Run(ctx) error`; whoever starts
  them waits for them (`sync.WaitGroup`) before returning.
- Select on `ctx.Done()` next to blocking channel operations; bound fan-out.
- A mutex sits next to the fields it guards; never hold a lock across I/O.
- `time.NewTicker` with `defer t.Stop()` in loops; no `time.After` in a loop.

## 8. Tests

- Test first for each behaviour; a bug fix starts with a failing regression test.
- Black-box tests: `package foo_test` **[checked]**. To reach an unexported helper, export it for
  tests in `export_test.go` rather than testing from inside the package.
- Table-driven with a named `t.Run` per case; the case type is named
  (`type verifyCase struct{…}`) and cases are named by behaviour (`"rejects a reordered entry"`).
  `t.Parallel()` where nothing is shared.
- Hand-written fakes for ports, honouring their contract (errors, `ctx` cancellation). A test TSA
  lives in `internal/tsa/tsatest`; tests that need `openssl` say so.
- Unit tests: no real network (use `httptest`), no files outside `t.TempDir()`, no wall clock
  (pass a fixed `ledger.Clock`).
- Tests that build and run the binary are behind `//go:build integration` (`make test-integration`).
- `go test -race -count=1 ./...` passes. Coverage of a touched package never goes down; it
  *should* be ≥ 70%.
- Never weaken, skip or delete a test to make it pass.

## 9. Checks before a PR

```
make check     # gofmt, go vet, go test -race with -tags=integration
make rules     # tools/rulecheck: the [checked] rules above
make crap      # tools/crap: fails on any function with CRAP >= 10
make lint      # golangci-lint with its default linters
make mutate    # gremlins mutation testing
```

CI runs `make check`, `make rules` and `make crap`. Every function stays under CRAP 10.

## 10. Naming

The project is ambermoth: binary `ambermoth`, environment variables `AMBERMOTH_*`, default
directory `.ambermoth`. The old working names (forerecord, provledger) are not used in new code.

## 11. ub-mode

Every change to Go code follows [ub-mode](https://github.com/unclebob/ub-mode), Robert C.
Martin's reduced principles: first make it work, then make it right, then follow practice.
Coding agents load the skill for every Go change. It is in `.claude/skills/ub-mode/`, copied
unchanged from upstream under its MIT license (see `SOURCE.md` there). Where the skill and these
rules differ, these rules win. Its steps map to this
repo like this:

| ub-mode step | Here |
|---|---|
| Crapper (CRAP < 10 per function) | `make crap`: CRAP = complexity² × (1 − coverage)³ + complexity, from `go test -coverprofile` |
| Dryer (duplication rank ≥ 0.8) | in review: remove the duplicate or say in the PR why it stays |
| Mutator | `make mutate` (gremlins); surviving mutants get a test or the code goes; `deadcode -test ./...` finds dead code |
| TDD | §8 |
| Boundary / type discipline | parse flags, config, provider responses and TSA tokens once at the edge; typed values inside |
| Idempotent operations | §5 |
| Separate before serializing shared state | §5 (one ledger writer) and §7 |
| Keep a log | the PR description says what was done, why, and how it was proved |
| Commit and push | small Conventional Commits on a branch, each saying why, what changed and how it was proved; then a PR to `main` (never push to `main` directly) |
