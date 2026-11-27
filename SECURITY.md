# Security policy

The proxy sees live API keys in transit. That makes it the most sensitive part of this project.

Report vulnerabilities privately through GitHub Security Advisories ("Report a vulnerability") on this repository. Please don't open a public issue. We aim to acknowledge reports within 72 hours.

A vulnerability that breaks any of these design commitments is in scope:

- API keys are never written to disk, the ledger or logs.
- Prompt and output content is never written. Only per-entry HMACs are.
- The ledger cannot be silently rewritten in a way that `verify` fails to detect.
- `verify-pack` cannot be made to report OK for a pack whose ledger segment, tokens, totals or `summary.html` were altered.
- `verify` and `verify-pack` reject a timestamp from a TSA that does not chain to a pinned root, unless the verifier passes `--allow-unpinned-tsa`.
