# ADR-0004: Write-ahead mode: record an intent before the call is sent

Status: Accepted · 2026-11-28 · signed off by Rafa

## Context
Today a call is recorded after the response ends, in a batch that is fsynced at
least every 200 ms (SPEC §6.4). A call can leave the machine and have no record
if the host crashes before the batch is written, if the queue is full, or if
the write fails and the batch is dropped. A `gap` entry counts such losses
afterwards, when the proxy lives to write one. A crash in the middle of a long
stream loses that call with no trace at all.

This is the weak spot of the ledger: "no other usage occurred" is what it does
not establish (SPEC, top). Write-ahead mode closes the part the proxy can.

## Decision
With `write_ahead: true` in `config.json` (default `false`), the proxy writes
an `intent` entry and fsyncs it before it forwards the request. When the call
ends, it writes the `call` entry as today, which completes the intent.

What it guarantees: every request the proxy forwarded has an `intent` entry on
disk, or the request was refused. A call cannot leave the machine through the
proxy without a record of it, and a dropped completion batch leaves its intent
behind instead of nothing.

What it does not:
- An intent with no completion (an orphan) does not say whether the request was
  sent. `kill -9` between the fsync and the forward leaves an intent for a call
  that never happened. Nothing can tell the two apart, so the ledger says
  "unknown" and no more.
- The intent has no status, tokens or output. A stream cut by a crash leaves an
  orphan, not a partial call. A stream cut by the client is a normal `call` with
  `client_aborted`.
- What the provider did with a request, or whether it billed it, is outside the
  ledger. Reconciliation (SPEC §10) is still the check on that.
- It covers traffic through the proxy, and trusts the disk to honour fsync.

### Entry shape
(a) A new `intent` entry, and the `call` entry as today with two added fields
that point back to it. (b) The `call` entry written early with unknown fields,
and a later `call_result` that fills them in.

(b) changes what a `call` is. Every consumer (totals, reconcile, alerts, the
pack) would have to merge two entries, and an older tool would count the early
entry with zero tokens and ignore the result: wrong totals, quietly. (a) leaves
`call` as it is. An older tool counts the same calls and tokens as before.

Chosen: (a). Both entries are chained and hashed like any other (SPEC §3.2);
the writer assigns seq in append order, so the intent always precedes its call.

`intent` fields, besides the common ones:

| Field | Notes |
|---|---|
| `call_id` | Random, 128 bits, hex. Unique per call |
| `provider`, `api_style`, `endpoint`, `method`, `path` | As in `call` |
| `key_fp` | As in `call` |
| `purpose` | Tag at the time, after route and key defaults |
| `salt_id` | The same value the call will carry |
| `declared_bytes_in` | `Content-Length` of the request; omitted if it has none |

`ts` is the start of the request, as on a call. The `call` entry gains
`call_id` and `intent_seq`, only when write-ahead is on. Everything else stays
in the `call`: `model`, `stream`, `status`, tokens, `latency_ms`, `note`,
`git_ref`, `transport`, `prompt_hmac`, `output_hmac`, `bytes_in`, `bytes_out`,
`client_aborted`.

The request MAC and `bytes_in` are not in the intent. The body is hashed while
it streams to the provider (SPEC §6.2), so they do not exist before the
forward, and holding the body back would delay every call by its upload time.
The intent proves that a request to this endpoint with this key was started at
this time, not what it held. The `call` entry binds the content, as today.

### Cost
One fsync in the request path. Its price is the disk's: usually 0.1 to 10 ms,
more on a network volume. Task 15 re-runs `make bench` with the mode on and
writes the numbers in `docs/benchmarks.md`. The < 20 ms p50 target is for the
default mode; with write-ahead on the measured figure is published, not gated.

Group commit keeps the cost from growing with load. Intents go through the
same queue as other entries, with a channel that the writer answers. The
writer flushes as soon as its batch holds an intent, so concurrent intents
share one fsync, along with any completions already queued. It never waits for
more to arrive: the forward is delayed by at most the fsync in progress plus
its own.

### When the intent cannot be written
Fail closed. With `write_ahead: true` the proxy refuses the call: it does not
forward, and it answers 503 with `{"error":"ambermoth: write-ahead failed, call
not sent"}`. This is a write error, a full queue, or no answer within 5 s.
A refused call is not recorded and not counted as dropped: nothing was sent.

This reverses the fail-open rule of SPEC §6.4, on purpose. A mode that sends
the call anyway when it cannot record it would give the same guarantee as no
mode. The trade-off is availability: a full or failing ledger disk stops AI
traffic through the proxy. Someone who cannot accept that leaves `write_ahead`
off. There is no second switch for fail-open write-ahead; the one flag has one
meaning.

A failed completion is still fail-open: the call has happened. The intent stays
as an orphan and the `gap` entry is written as now.

### Orphans
- `verify` reports the count and the list (seq, ts, endpoint) as information.
  It is not a warning and does not change the exit status. A proxy that is
  running has intents in flight; the report says so.
- A `call` whose `intent_seq` is not an `intent` with the same `call_id`, or an
  intent completed twice, is an error: the chain is intact but the records
  contradict each other. A call whose intent lies before a segment is not an
  error.
- Packs and `report`: the summary and the evidence letter count orphans on
  their own line, apart from calls: "N intents have no call entry in this
  segment. Each is a request the proxy began to send. Whether it was sent, or
  answered, is not known." Totals come from `call` entries only. An intent near
  the end of a segment may be completed just after it; the list shows seq and
  ts so a reviewer can see that.
- `reconcile` leaves its totals alone and prints the orphan count for the period
  next to a mismatch, as a hint: unrecorded calls show as export above ledger.
  The `reconciliation` entry is unchanged.
- `alerts` ignores orphans: they carry no volume, and `verify` shows them.
- `repair` is unchanged. A torn last line that is an intent means the call was
  not sent, since the forward waits for the fsync: removing it loses nothing. A
  torn completion leaves its intent as an orphan.
- Nothing closes an orphan, not even a restart or a `gap`: a "closed" entry
  could only say "unknown" again. The proxy logs the open intents at start.

### Format impact
New entry type `intent`, and two optional fields on `call`. SPEC §4 changes in
task 15, as CONTRIBUTING requires.

What the current tools do with it: `verify` checks no type and no value of `v`,
so a ledger with intents passes the same checks. `report`, `pack`, `reconcile`
and `alerts` switch on type and skip what they do not know. An older release
therefore gives the same chain result and the same totals, and does not see the
orphans. Nothing it says becomes false; it says less. No change to shipped
releases can make it fail.

- Ledger stays `ambermoth-ledger/1`, `v` stays 1. A genesis flag is no use:
  genesis cannot be edited, and the mode can be turned on in an old ledger.
- A pack whose segment holds an `intent` entry is `ambermoth-pack/2`. Any other
  pack stays `/1`, byte for byte as now. An older `verify-pack` refuses `/2` by
  name (`unsupported pack format`), so a reviewer on an old tool is told to
  upgrade instead of reading a summary that leaves orphans out. The new
  verifier accepts both.
- This is not a breaking change in CONTRIBUTING's sense: every existing ledger
  and pack verifies as before, and the mode is opt-in. Task 15 is `feat(proxy)`,
  not `feat!`.

## Alternatives rejected
- fsync every entry, no intents. It makes each call durable after the response,
  which is the late end. A crash during the call still leaves nothing, and a
  long stream is exactly where that happens.
- A sidecar journal. A second file to lock, verify and ship in packs, outside
  the hash chain and the anchors. Two sources of truth (ADR-0002).
- The provider's own logs. Not in the customer's hands, kept for a limited
  time, and the reason for this ledger is not to depend on them. Reconcile
  already compares against their exports.
- Recording the whole call synchronously. It holds the last byte until an fsync
  and needs the end of the stream first, so it cannot cover the crash either.

## Consequences
Default behaviour, entries and packs do not change. With the mode on, the ledger
grows by one entry (about 300 bytes) per call, and the ledger disk becomes part
of the availability of AI calls.

## Task 15 will
- Add `TypeIntent`, the `call_id` and `intent_seq` fields, and `write_ahead` in
  `Config` with its validation.
- Writer: queue items with an optional ack channel, flush on intent, one fsync
  per batch, a 5 s limit, and 503 on failure. A WebSocket upgrade gets its
  intent before the upgrade is forwarded.
- A seam in `ledger.Store` for tests to make fsync fail (the plan notes there is
  none), and tests for fail-closed, group commit, a crash between fsync and
  forward, a stream cut mid-way, and a torn intent under `repair`.
- `verify` (streaming, open set of call ids), the orphan lines in the pack
  summary and letter, `report` and `reconcile`, and `ambermoth-pack/2` in
  `pack` and `verify-pack`. Existing texts in `internal/pack/texts.go` are not
  changed; new wording is added beside them.
- Re-run `make bench` with the mode on; add the numbers to `docs/benchmarks.md`.
- SPEC §4.2, a new §4.9 (`intent`), §6.4, §8, §9 and §12; tick the ROADMAP item.

## Open for sign-off
1. Fail closed (503, no forward) when `write_ahead` is on, with a fixed 5 s
   limit. Recommended: yes.
2. The intent carries no prompt MAC or `bytes_in`, only `declared_bytes_in`,
   instead of buffering the request body up to a cap. Recommended: no
   buffering.
3. `ambermoth-pack/2` only for packs that hold an intent, so old verifiers fail
   loudly. Recommended: yes. The alternative is `/1` with an added manifest
   count, which an old tool would ignore.
4. Orphans stay open for good, with no closing entry after a crash.
   Recommended: yes.
