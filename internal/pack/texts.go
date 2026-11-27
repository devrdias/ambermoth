package pack

// LimitsText is the limits statement shipped in every pack (SPEC preamble).
// Changing it is a pack-format change.
const LimitsText = `# What this evidence pack does and does not establish

## Establishes
- The ledger entries in this pack existed, in this order, no later than the
  times given by the independent RFC 3161 Time Stamping Authorities whose
  tokens are included.
- The entries have not been altered since they were anchored: any edit,
  deletion, insertion or reordering breaks the hash chain, which the included
  open-source verifier checks.
- The declared purposes and code links were recorded at the time of the calls,
  or their later amendment is itself on record (shown in this pack).
- The usage totals in the summary are recomputed by the verifier from the
  ledger entries themselves.

## Does not establish
- That no other, unlogged usage occurred — for example on API keys or accounts
  that were not routed through this ledger. This is mitigated, not eliminated,
  by key enrolment records and billing reconciliation (see Coverage).
- That a purpose tag was truthful when written. This is mitigated by code
  linkage (repository and commit history) and by volume consistency.
- Entries after the last anchor are self-attested only.
- That the provider must accept any of it.

The ledger stores keyed hashes (HMAC-SHA256) of prompts and outputs, never
their content. Content for individual entries can be checked only where the
sender has chosen to disclose that entry's key together with the original
request and response.

This pack is a record, not a certification of innocence, and not legal advice.
`

const verifyText = `# Verifying this pack

You do not need to trust the sender or the vendor of this tool.

1. Get the open-source verifier (standard library Go, no dependencies):

       git clone https://github.com/devrdias/ambermoth
       cd ambermoth && go build ./cmd/ambermoth

2. Verify the pack offline and write the verifier's own report:

       ./ambermoth verify-pack pack.zip --report report.html

   Read report.html, not the sender's summary.html: the verifier renders it
   from the ledger entries themselves. (verify-pack also checks that the
   sender's summary.html is identical to that render.)

   Timestamps must come from a pinned Time Stamping Authority. The verifier
   pins FreeTSA and DigiCert by default; add another root with
   --tsa-ca root.pem. Tokens from any other authority fail verification.

3. What is checked: every file's SHA-256 against manifest.json; canonical form,
   sequence and hash chain of ledger/segment.jsonl; each RFC 3161 token's
   imprint, CMS signature and certificate chain to a pinned root; the usage
   totals, recomputed from the entries; and summary.html, re-rendered from the
   verified data. The report also shows the anchor lag: how long after its own
   timestamp each entry was first sealed by a TSA.

4. Checking a disclosed sample: if the sender gave you the original request
   and response for an entry listed in disclosures/entry-keys.json:

       ./ambermoth check-sample --pack pack.zip --seq <N> --request req.json --response resp.txt

Independent re-implementation is welcome: the format is specified in
docs/SPEC.md of the repository.
`

const letterTmpl = `To the {{.Provider}} review team,

Re: API usage on account {{.Account}} — {{.From}} to {{.To}}

We understand our account has been flagged for possible distillation. We do not
train models on {{.Provider}} outputs. This letter and the attached evidence
pack describe our usage for the period.

**What the traffic is.** In this period we made {{.Calls}} API calls through
{{.Keys}} enrolled key(s). The largest class, {{.TopShare}}% of calls, is
"{{.TopPurpose}}": [ONE SENTENCE: what produces it and why — e.g. "our
regression benchmark runs on every change to our prompt settings"].
{{if .TopLink}}It is produced by {{.TopLink}}, whose history begins {{.TopLinkSince}}.{{end}}

**How it was recorded.** Every call was recorded at the time by an
append-only, hash-chained ledger, anchored to independent RFC 3161
timestamp authorities. The pack verifies offline with an open-source tool
(see VERIFY.md). It contains hashes, not our users' content.

**Keys.** [State any rotations or suspected-compromise actions, with dates —
see the Key history section.]

**What would help us respond precisely.** Could you share the date range,
key, or traffic characteristics behind the flag? We can then point to the
specific entries and, where useful, disclose individual samples.

The pack's limits are stated plainly in LIMITS.md.

Regards,
[Name, role, contact]
`
