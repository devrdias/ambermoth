# ADR-0003: Pass-through of the customer's own keys only

Status: Accepted · 2026-11-25 · Re-check provider terms every release

## Context
OpenAI's November 2026 usage policy requires review/approval for
gateway/intermediary platforms that provide model access to third parties.

## Decision
ambermoth never holds, pools, resells or substitutes API keys. It forwards the
caller's own credentials unchanged to the provider the caller chose, for the
caller's own applications. It runs in the caller's own infrastructure.

## Consequences
- No "bring one key, serve many customers" feature, ever.
