# ambermoth — Technical Specification

Status: Draft v0.1 · 25 Nov 2026 · Ledger format `ambermoth-ledger/1` · Pack format `ambermoth-pack/1`

This document turns the product spec ("AI-Usage Provenance Ledger", v0.1) into
exact, implementable rules. Anyone should be able to write a second, independent
verifier from this document alone. Where this document and the code disagree,
this document is the intended behaviour and the code has a bug.

The key words MUST, SHOULD and MAY are used as in RFC 2119.

> What this system proves, and what it doesn't (repeated verbatim in every
> evidence pack, see §9):
>
> *Establishes:* these entries existed, in this order, no later than the
> anchored times; they have not been altered since; the declared purposes and
> code links were recorded at the time (or their later amendment is itself on
> record).
>
> *Does not establish:* that no other, unlogged usage occurred (mitigated, not
> eliminated, by billing reconciliation); that a purpose tag was truthful when
> written (mitigated by code linkage and volume consistency); or that the
> provider must accept any of it.

---

## 1. Components

```
App ──► ambermoth proxy ──► Provider API (OpenAI / Anthropic / OpenAI-compatible)
              │
              ├─ entry builder  (HMAC prompt/output, purpose, key fingerprint)
              ├─ ledger writer  (append-only JSONL, hash chain, fsync)
              └─ anchor worker  (RFC 3161 TSA, hash only)   [optional, interval]

ambermoth CLI:  init · proxy · key · amend · link · anchor · verify
                 pack · verify-pack · reconcile · alerts · report
```

One static Go binary, standard library only. No database server, no queue.
The verifier is the same binary (`ambermoth verify`, `ambermoth verify-pack`)
and builds offline from the repository.

## 2. Ledger directory

```
.ambermoth/
  config.json        proxy + policy configuration (not part of the evidence)
  secret.key         32-byte HMAC master key, hex, mode 0600 — customer-held, never leaves
  ledger.jsonl       the ledger: one canonical JSON entry per line, append-only
  ledger.lock        advisory lock file (flock) for writers
  anchors/           RFC 3161 tokens: <seq>-<n>.tsr (DER TimeStampResp)
```

`ambermoth init` creates `secret.key` and `ledger.jsonl` only if neither exists.
It never replaces a key: the key is what every entry HMAC derives from, and a
ledger kept elsewhere could no longer be disclosed from. An existing
`config.json` and `anchors/` are kept.

The ledger is a plain file on purpose: it can be inspected with `cat`, copied,
backed up, and verified without the software that wrote it.
See [ADR-0002](adr/0002-jsonl-not-sqlite.md).

## 3. Canonical form and hashing

### 3.1 Canonical JSON

Every ledger line MUST be the canonical serialisation of the entry object:
a strict subset of RFC 8785 (JCS):

- Object keys sorted by UTF-16 code units, no duplicates.
- No insignificant whitespace.
- Strings: UTF-8; escape only `"` `\` and control characters U+0000–U+001F
  (`\b \f \n \r \t` short forms, others as `\u00xx` lowercase hex).
  `<`, `>`, `&` and non-ASCII are written literally.
- Numbers: integers only, in `[-(2^53-1), 2^53-1]`, no leading zeros,
  no exponent, no `-0`. Non-integers are not allowed anywhere in an entry.
- `true`, `false`, `null` as literals.

A verifier MUST reject a line whose bytes differ from the canonical
serialisation of its own parsed value.

### 3.2 Hash encodings

All digests are written `"<alg>:<lowercase hex>"`, e.g. `sha256:9f2c…`.

- `entry_hash = "sha256:" + hex(SHA-256(canonical(entry without "entry_hash")))`
- `prev_hash` of the genesis entry is `"sha256:" + 64 × "0"`.
- For every other entry, `prev_hash` MUST equal the previous line's `entry_hash`.
- `seq` starts at 0 (genesis) and MUST increase by exactly 1 per line.

Unknown fields are allowed (forward compatibility) and are covered by the hash.

### 3.3 Content HMACs (privacy)

Raw prompts and outputs are never stored. Instead:

```
entry_key   = HMAC-SHA256(master_key, "ambermoth/entry-key/v1:" || salt_id)
prompt_hmac = "hmac-sha256:" + hex(HMAC-SHA256(entry_key, request_body_bytes))
output_hmac = "hmac-sha256:" + hex(HMAC-SHA256(entry_key, response_body_bytes))
```

- `salt_id` is 16 random bytes (hex), fresh per call, stored in the entry.
- `request_body_bytes` are the exact bytes forwarded upstream;
  `response_body_bytes` are the exact bytes returned to the client
  (for SSE, the entire raw event stream).
- Because each entry has its own derived key, the customer can disclose
  `entry_key` for chosen entries only (`pack --disclose`). A reviewer who is
  also given the original request/response bytes can then confirm they match
  the ledger, while every other entry stays brute-force resistant.
- The genesis entry commits to the master key:
  `hmac_key_commitment = "sha256:" + hex(SHA-256("ambermoth/key-commit/v1:" || master_key))`.

### 3.4 Key fingerprints

`key_fp = "sha256:" + hex(SHA-256(api_key_utf8))`. The API key itself is never
written anywhere. A plain (unkeyed) hash is deliberate: provider keys are
high-entropy, and the provider can compute the same fingerprint for the keys it
issued, so a reviewer can match fingerprints to accounts without the customer
re-sending secrets.

## 4. Entry types

Common fields on every entry:

| Field | Type | Notes |
|---|---|---|
| `v` | int | Ledger format version, `1` |
| `seq` | int | Monotonic per ledger |
| `ts` | string | UTC, RFC 3339 with milliseconds, `Z` suffix (writer's clock) |
| `type` | string | One of the types below |
| `prev_hash` | string | §3.2 |
| `entry_hash` | string | §3.2 |

### 4.1 `genesis` (seq 0)

`ledger_id` (UUIDv4), `hmac_key_commitment`, `generator` (e.g. `ambermoth/0.1.0`).

### 4.2 `call` — one upstream API request

| Field | Notes |
|---|---|
| `provider` | Upstream name from config: `openai`, `anthropic`, or a custom name |
| `api_style` | `openai` or `anthropic` (how the body was parsed) |
| `endpoint` | Normalised class: `chat.completions`, `responses`, `completions`, `embeddings`, `messages`, `messages.count_tokens`, `batches`, `files`, `models`, `realtime`, `other` |
| `method`, `path` | HTTP method and upstream path (no query string) |
| `model` | From request body `model`, else response body, else `""` |
| `stream` | bool |
| `status` | Upstream HTTP status; `0` if upstream unreachable |
| `tokens_in`, `tokens_out`, `cached_in` | From provider usage fields (§6.3); `0` if absent |
| `usage_reported` | bool — whether the provider returned usage at all |
| `latency_ms` | Request start → last response byte |
| `key_fp` | §3.4, or `""` if the call carried no key |
| `purpose` | Tag at the time of the call (§5) |
| `note` | Optional free text (`X-Provenance-Note`), at most 512 bytes in total |
| `git_ref` | Optional commit (`X-Provenance-Git-Ref`), at most 256 bytes |
| `transport` | Present only for upgraded connections: `websocket` (§6.5) |
| `salt_id`, `prompt_hmac`, `output_hmac` | §3.3 |
| `bytes_in`, `bytes_out` | Body sizes |
| `client_aborted` | bool — client disconnected before the response finished |

Text taken from request headers or the path is written as valid UTF-8: each
invalid byte sequence becomes U+FFFD. A value over its limit is cut on a
character boundary and ends in `…[truncated]`, which counts towards the limit.

When the purpose header is invalid (§5), the proxy adds `invalid purpose header:
<value>` to the note, after the caller's own note and a space. The whole note
still fits in 512 bytes. The purpose text is guaranteed 256 bytes (or all of
it, if shorter), the caller's note gets the rest; the fixed prefix is never
cut, only the value.

### 4.3 `amendment` — re-tag or annotate earlier entries

`target_from`, `target_to` (inclusive seq range of `call` entries),
`new_purpose` (optional), `annotation` (optional), `reason` (required),
`by` (required, free text: who). Originals are never modified. The
*effective purpose* of a call is its `purpose`, replaced by each later
amendment that covers it, in ledger order. Packs show both.

### 4.4 `key_event`

`action` ∈ `enroll | rotate | revoke | auto_enroll`, `key_fp`, `provider`,
`label`, `reason`, and for `rotate` the `replaced_by` fingerprint.
`auto_enroll` is written by the proxy the first time it sees an unknown key
(policy `auto_enroll: true`, default), so enrolment is never silently skipped.

### 4.5 `code_link`

Binds a purpose to code: `purpose`, `repo` (remote URL or path), `commit`,
`first_commit_ts`, `commit_count`, `path` (optional subdirectory).
Written by `ambermoth link` — typically from CI on every eval-harness change.

### 4.6 `anchor`

`target_seq`, `target_hash` (the anchored `entry_hash`), `tsa` (URL),
`token_file` (relative path under `anchors/`), `token_sha256`, `gen_time`
(the TSA's time, copied for readability — the token is authoritative).

### 4.7 `gap`

Written when the proxy failed to record calls (fail-open, §6.4):
`dropped` (count), `since`, `until`, `cause`. Gaps are shown in every pack.

### 4.8 `reconciliation`

Result of comparing the ledger with a provider usage export:
`provider`, `period_from`, `period_to`, `source_sha256` (hash of the export
file), `ledger_tokens_in/out`, `export_tokens_in/out`, `days_mismatched`,
`result` ∈ `match | mismatch`.

## 5. Purpose tags

Taxonomy v1: `production-inference`, `eval-regression`, `eval-benchmark`,
`rag-indexing`, `rag-query`, `batch-job`, `dev-experiment`, `other-declared`
(requires a note). Custom tags MUST start with `x-`. The proxy writes
`untagged` when nothing applies.

Resolution order for a call: `X-Provenance-Purpose` header → route rule
(path prefix match in config) → per-key default (by fingerprint) → config
default → `untagged`. An invalid header value is recorded as `untagged` with
`note: "invalid purpose header: <value>"` — never rejected (fail open).

Purposes in `config.json` (`default_purpose`, the `purpose` of each route rule,
the values of `key_defaults`) are checked when the config loads, with the same
rule as the header: a taxonomy tag, `untagged` or an `x-` tag. `other-declared`
is refused there, because it needs a note and a config has none. An empty
`default_purpose` means `untagged`. A config with any other value does not
load, and `ambermoth proxy` refuses to start; the error names the field and the
value.

## 6. Proxy behaviour

### 6.1 Routing

`http://<listen>/<upstream-name>/<path>` → `<upstream.base_url>/<path>`.
Defaults: `openai → https://api.openai.com`, `anthropic → https://api.anthropic.com`.
SDK setup is a single base-URL change, e.g. `OPENAI_BASE_URL=http://localhost:8787/openai/v1`,
`ANTHROPIC_BASE_URL=http://localhost:8787/anthropic`.

### 6.2 Pass-through guarantees

- Request and response bodies are forwarded byte-for-byte; unknown fields untouched.
- Responses are streamed with immediate flushing (no buffering of SSE).
- All `X-Provenance-*` request headers are removed before forwarding.
- The proxy never pools, resells or substitutes keys: it forwards the caller's
  own credentials unchanged. (This is the design constraint that keeps it out of
  "gateway/intermediary" territory — see ADR-0003.)
- API keys exist only in memory for the duration of the request.

### 6.3 Usage extraction

| Style | Non-streaming JSON | Streaming SSE |
|---|---|---|
| OpenAI chat/completions | `usage.prompt_tokens`, `usage.completion_tokens`, `usage.prompt_tokens_details.cached_tokens` | last chunk with `usage` (requires `stream_options.include_usage`) |
| OpenAI responses | `usage.input_tokens`, `usage.output_tokens`, `usage.input_tokens_details.cached_tokens` | `response.completed` event → `response.usage` |
| OpenAI embeddings | `usage.prompt_tokens` | — |
| Anthropic messages | `usage.input_tokens`, `usage.output_tokens`, `usage.cache_read_input_tokens` | `message_start.message.usage` + max of `message_delta.usage.output_tokens` |

Parsing is best-effort and bounded (the proxy keeps at most 4 MiB of a
non-streaming body and one SSE line at a time for parsing; hashing always
covers the full body).

### 6.4 Failure modes (fail open)

- Ledger write failure or a full write queue MUST NOT block or fail the app's
  request. The writer counts dropped entries and, on the next successful write,
  first appends a `gap` entry.
- Durability: entries are written by a single writer goroutine, batched, and
  `fsync`ed at least every 200 ms. This is best-effort durability (an entry
  may be lost if the host crashes within that window); a crash leaves at most a
  truncated final line, which `verify` reports and `ambermoth repair` (future)
  truncates with a `gap` entry. Write-ahead mode is future work.
- Overhead target: < 20 ms p50 on non-streaming calls (HMAC is computed
  incrementally while bytes stream; no extra round trips).
- Shutdown: on SIGINT/SIGTERM the proxy stops accepting connections, waits up
  to 30 s for in-flight calls (including long streams) to finish and be
  recorded, closes upgraded connections (which records them), then drains the
  writer. A call that outlives the grace period is appended synchronously when
  it ends; an entry is never silently lost at shutdown.
- If an `auto_enroll` entry is dropped, the key is not marked known, so the
  enrolment is retried on that key's next call.

### 6.5 Upgraded connections (WebSocket)

A response with status `101 Switching Protocols` (e.g. the OpenAI Realtime API)
is recorded as one `call` entry per connection, written when the connection
closes: `status` 101, `endpoint` `realtime`, `transport` `websocket`,
`bytes_in`/`prompt_hmac` over everything the client sent after the upgrade and
`bytes_out`/`output_hmac` over everything the upstream sent. Frames are not
parsed in v0.1, so `tokens_in`/`tokens_out` are 0 and `usage_reported` is
false; billing reconciliation (§10) covers the token totals.

## 7. Anchoring (RFC 3161)

`ambermoth anchor [--tsa URL]` (or the proxy's `anchor_interval`):

1. Read the current head entry (seq *n*, `entry_hash` *h*).
2. Send a `TimeStampReq` with `messageImprint = (SHA-256, raw 32 bytes of h)`,
   a random 64-bit nonce and `certReq = true`. Only the hash leaves the machine.
3. Validate the response: status granted, nonce echoed, imprint equals *h*.
4. Store the DER response as `anchors/<n>-<k>.tsr` and append an `anchor` entry.

A verifier checks each token:

- status is granted or granted with modifications; the imprint is SHA-256 of *h*.
- the CMS signature over the TSTInfo verifies with the certificate in the token.
- the SignerInfo has signed attributes (RFC 5652 §5.3), and their messageDigest and content-type match the TSTInfo.
- a signing-certificate attribute (ESSCertID or ESSCertIDv2) is present, and its hash, and issuerSerial if given, match the certificate that verified the signature.
- with pinned roots, the signer certificate chains to a root at `genTime` and carries the timeStamping extended key usage; a certificate with no extended key usage fails.

Multiple independent TSAs SHOULD be used. `ambermoth init` configures two:
`https://freetsa.org/tsr` and `http://timestamp.digicert.com`. Each anchor run
requests a token from every configured TSA; a TSA that fails does not prevent
the others from anchoring (the command reports the partial failure).

The binary embeds the root certificates of the default TSAs
(`internal/trust/roots/`, with SHA-256 fingerprints pinned in tests):

| File | Root |
|---|---|
| `freetsa-root.pem` | FreeTSA root CA |
| `digicert-trusted-root-g4.pem` | DigiCert Trusted Root G4 |
| `digicert-assured-id-root-ca.pem` | DigiCert Assured ID Root CA |

Extra roots are added with `--tsa-ca root.pem` (repeatable). A token whose
signing certificate does not chain to a pinned root, with the timeStamping EKU,
fails verification unless `--allow-unpinned-tsa` is given, in which case it
is accepted with a warning and shown as "NOT PINNED". A self-signed or
attacker-run TSA is therefore rejected by default.

An anchor on seq *n* proves entries 0…*n* existed in exactly this
form no later than the TSA's `genTime`. Rewriting history afterwards would
require a new token with a past `genTime`, which an honest TSA will not issue.

Phase 2, public anchoring: daily head digest published to an append-only
public log (RFC 9162-style) to close forking (showing different chains to
different audiences). The `anchor` entry carries a `kind` field reserved for this.

## 8. Verification algorithm (`ambermoth verify`)

A verifier MUST check, in order, and report every failure (not stop at the first):

1. Syntax. Each line parses; is canonical (§3.1); has the common fields.
2. Chain. `seq` contiguous from the first line; `prev_hash` links;
   recomputed `entry_hash` matches. A segment not starting at genesis is
   verified relative to its first line's `prev_hash`.
3. Genesis. If present: seq 0, zero `prev_hash`, has `ledger_id`.
4. Time sanity. `ts` non-decreasing beyond a 5 s tolerance (warning, not error).
5. Anchors. For each `anchor` entry: token file present, `token_sha256`
   matches, token parses, status granted, imprint equals the `entry_hash` of
   `target_seq` (which must be in the verified range), CMS signature valid over
   the TSTInfo with the embedded TSA certificate, and the certificate chains to
   a pinned root (§7) with the timeStamping EKU (unless `--allow-unpinned-tsa`).
   Entries whose `ts` is later than the `genTime` anchoring them (beyond
   tolerance) are flagged.
6. Coverage and anchor lag. Report the number of trailing entries not yet
   anchored and, for every anchored entry, its *lag*: the earliest TSA
   `genTime` of any anchor covering it minus its own `ts`. An entry is only
   proven to exist from that time on, so a ledger anchored once after weeks of
   writing proves little about when each call happened. The report gives the
   largest lag (and its seq) and the number of entries with a lag over 24 h,
   and warns when that number is not zero.
7. Gaps. List all `gap` entries.

Exit status 0 only if steps 1, 2, 3 and 5 pass.

## 9. Evidence pack (`ambermoth-pack/1`)

`ambermoth pack --from DATE --to DATE [--provider P] [--disclose seq,…] [--tsa URL] --out pack.zip`

### 9.1 Segment selection

The pack contains a *contiguous* ledger segment so the reviewer can verify it:

- start = the latest anchored seq ≤ the first entry in the period (or genesis);
- end = the earliest anchored seq ≥ the last relevant entry (the last call in
  the period, or a later amendment that re-tags one of those calls), extended to
  include every `anchor` entry for that seq (one per TSA, so the pack carries
  independent timestamps) — or the head, reported as "end not anchored".

Both ends are therefore pinned to TSA time. Entries from other providers inside
the segment are included (they are metadata only) but excluded from the
itemised totals when `--provider` is set. The genesis entry is always included
as `ledger/genesis.json` so `ledger_id` and the key commitment are known.

### 9.2 Layout

```
pack.zip
  manifest.json            format, ledger_id, period, filter, segment range,
                           totals (machine-readable), files[] with sha256
  manifest.tsr             optional RFC 3161 token over sha256(manifest.json)
  summary.html             human-readable report (Appendix A items 1–8),
                           a deterministic render of the data (§9.3)
  LIMITS.md                the limits statement, verbatim
  VERIFY.md                how to verify offline
  letter-template.md       narrative letter to the provider's review team
  ledger/genesis.json
  ledger/segment.jsonl
  anchors/*.tsr
  disclosures/entry-keys.json   only with --disclose: {seq: entry_key hex}
```

### 9.3 `verify-pack`

`ambermoth verify-pack pack.zip [--report report.html] [--tsa-ca root.pem] [--allow-unpinned-tsa]`

Checks every `files[]` hash and that the pack holds every file §9.2 defines
for it: `manifest.json`, `summary.html`, `LIMITS.md`, `VERIFY.md`,
`letter-template.md`, `ledger/genesis.json`, `ledger/segment.jsonl`, the token
of every anchor entry in the segment, `ledger/context.jsonl` when
`context_entries` is not zero (and then exactly that many entries) and
`disclosures/entry-keys.json` when `disclosed` is not empty (with a key for each
of them and for no others). The sender writes `files[]`, so a file it leaves out
of the list is not excused. Runs §8 on `ledger/segment.jsonl` with the pack's
anchors, checks `genesis.json` hash/fields, recomputes the totals from the
segment and compares them with `manifest.totals`, and validates
`manifest.tsr` (against the pinned roots) if present.

File hashes alone do not bind `summary.html` to the data:
the sender controls `manifest.json` too and could edit the summary and update
its hash. So `summary.html` is a deterministic function of the manifest (with
recomputed totals), the context and segment entries, and the segment's
verification result. `verify-pack` renders it again and requires the sender's
copy to be byte-identical; any edit fails verification. If the pack was made by
a different ambermoth version (`manifest.generator`), the byte comparison is
skipped with a warning.

`--report` writes the summary as rendered by the
verifier, from the verified data, with a banner stating the result, the
verifier version and any errors or warnings. Reviewers should read this file,
not the sender's `summary.html`. The sender's own explanation of the traffic
belongs in `letter-template.md`, which is labelled as the sender's account.

`key_event`, `code_link` and `reconciliation` entries from
before the segment are included in `ledger/context.jsonl`. Each is internally
consistent, but the pack cannot prove where it sat in the chain, so the summary
labels them "before the segment · self-attested".

A pack is at most 1 GiB uncompressed; larger archives are refused, and so is
one with a zip entry whose name is not a plain relative path (a `..` or empty
part, a leading `/`, a backslash), a name that appears twice, or an entry that
is not a regular file (a symlink, say). `ambermoth pack` takes the token of an
anchor entry only from a `token_file` that is a plain file name, and refuses
one with a directory part. A line of
`ledger/segment.jsonl`, `ledger/context.jsonl` or `ledger/genesis.json` is at
most 16 MiB; a longer one is a failed check that names the file and the line,
not a command error.

### 9.4 Contents map (product spec Appendix A)

| # | Item | Source in pack |
|---|---|---|
| 1 | Cover sheet | `summary.html` §1, `manifest.json` |
| 2 | Coverage statement | keys seen vs enrolled, `gap` entries, latest `reconciliation` |
| 3 | Itemised usage | per effective purpose: calls, tokens, by day, endpoint mix, exemplar hashes |
| 4 | Flagged-pattern explanation | largest-share purpose highlighted; the sender's explanation is written in the letter |
| 5 | Code provenance | `code_link` entries per purpose |
| 6 | Key history | `key_event` entries |
| 7 | Integrity bundle | `ledger/`, `anchors/`, `VERIFY.md` |
| 8 | Limits statement | `LIMITS.md`, also in `summary.html` |
| 9 | Narrative letter | `letter-template.md` |

## 10. Reconciliation and alerts

- `ambermoth reconcile --provider P --csv usage.csv` — CSV with header
  `date,model,input_tokens,output_tokens` (adapters for each provider's export
  format are tracked as issues). Per-day totals are compared with a tolerance
  (default 2%); the result is appended as a `reconciliation` entry.
- `ambermoth alerts` — advisory only, phrased as "review this":
  - *volume*: a key/purpose's daily calls > 3× its trailing 14-day median;
  - *concentration*: one endpoint/purpose > 80% of a key's last-7-day calls
    with > 1,000 calls (thresholds configurable);
  - *new key*: an `auto_enroll` event in the window.
  There is no webhook or email delivery; the output is the terminal.

## 11. Threat model (summary)

| Attacker goal | Closed by | Residual |
|---|---|---|
| Edit/delete/insert an entry later | hash chain | — |
| Rewrite the whole chain and backdate it | RFC 3161 anchors from pinned TSAs; anchor lag reported | entries after the last anchor are only self-attested; a large lag weakens the timing claim |
| Anchor with a TSA they control | pinned roots; unpinned tokens fail by default | `--allow-unpinned-tsa` is the verifier's explicit choice |
| Show different chains to different reviewers | Phase 2 public anchoring | open in v0.1 |
| Hide traffic on an unlogged key | billing reconciliation, auto-enroll | other accounts/orgs are out of reach |
| Lie in a purpose tag | code links, volume consistency, amendment trail | a tag is a declaration, not proof |
| Brute-force short prompts from hashes | per-entry HMAC keys, master key never disclosed | — |
| Steal API keys from the proxy | keys never persisted; proxy is the top review priority | process memory compromise |
| Forge summary numbers or text | `verify-pack` recomputes totals and re-renders `summary.html`; `--report` gives the verifier's own render | the letter is the sender's account |

## 12. Out of scope for v0.1

SDK wrappers (Bedrock/Azure), Gemini-native API (use its OpenAI-compatible
endpoint), content escrow, public transparency anchoring, dashboards, SSO,
write-ahead durability.
