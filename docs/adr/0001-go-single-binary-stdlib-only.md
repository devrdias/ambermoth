# ADR-0001: Go, single binary, standard library only

Status: Accepted · 2026-11-25

## Context
The product spec asks for "single-language, boring, deployable as one binary",
and a verifier that "must build and run offline from the repo alone". The
verifier is what a provider's reviewer runs; every dependency is something they
would have to trust or audit.

## Decision
Go 1.22+, standard library only — including SHA-256, HMAC, X.509, ASN.1 (for
RFC 3161 / CMS parsing) and the reverse proxy. No third-party modules.

## Consequences
- `go build ./...` works offline from a clean checkout; `go.sum` is empty.
- We hand-write a small amount of ASN.1/CMS parsing (~300 lines) instead of
  importing a PKCS#7 library. It is covered by tests against tokens produced by
  `openssl ts`.
- Cross-compiles to every platform a reviewer might use.
