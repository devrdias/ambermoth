# Security policy

The proxy sees live API keys in transit. That makes it the most sensitive part of this project.

Report vulnerabilities privately through GitHub Security Advisories ("Report a vulnerability") on this repository. Please don't open a public issue. We aim to acknowledge reports within 72 hours.

A vulnerability that breaks any of these design commitments is in scope:

- API keys are never written to disk, the ledger or logs.
- Prompt and output content is never written. Only per-entry HMACs are.
- The ledger cannot be silently rewritten in a way that `verify` fails to detect.
- `verify-pack` cannot be made to report OK for a pack whose ledger segment, tokens, totals or `summary.html` were altered.
- `verify` and `verify-pack` reject a timestamp from a TSA that does not chain to a pinned root, unless the verifier passes `--allow-unpinned-tsa`.

## Scope for a security review

The commitments above are what to test. The code that reads bytes someone else controls is where to start:

| Input | Comes from | Code | Fuzz test |
|---|---|---|---|
| Ledger lines | a ledger or pack handed to `verify` | `ledger.Read`, `EachLine`, `ParseEntry`, `VerifyReader` | `FuzzLedgerLines`, `FuzzEachLine`, `FuzzParseEntry` |
| RFC 3161 token | a TSA reply, or a pack | `tsa.Verify` | `FuzzVerify` |
| Response stream | the provider, through the proxy | the SSE parser in `internal/proxy/usage.go` | `FuzzSSEUsage` |
| Canonical JSON | every entry | `internal/canon` | two differential fuzz tests |

Each test checks that nothing panics and that what is accepted is what was written: a ledger that verifies is canonical, hashed and chained, and a token that verifies is the one the TSA issued. Run one for a while with, for example, `go test ./internal/tsa -run '^$' -fuzz '^FuzzVerify$' -fuzztime 5m`. The first minute of a run can show no executions while Go minimizes its first find. `-fuzzminimizetime 3s` shortens that.

Known and not changed: `tsa.Verify` does not check the tag of some container elements, such as the outer SEQUENCE of the reply, the status SEQUENCE and the explicit `[0]` around the token. A reply with one of those tags altered still verifies. The signature, the TSTInfo and the imprint are still checked, so no attested fact changes, but a reviewer should know the verifier is not strict DER there. `FuzzVerify` allows it and checks that whatever verifies carries the signature and the TSTInfo that were issued.

Not fuzzed yet: reading and verifying the zip of a pack (`pack.ReadPack`, `pack.VerifyPack`), the request body prefix the proxy parses for `model` and `stream`, config loading (`proxy.LoadConfig`) and the CLI flags.

Out of scope: the TSAs themselves, the providers' APIs, and anyone who can already read the master key or write to the ledger directory. The last one can delete a ledger; `verify` is there to show what was written, not to stop that.
