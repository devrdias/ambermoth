# ADR-0002: Append-only JSONL file instead of SQLite as the ledger

Status: Accepted · 2026-11-25 (deviates from product spec §9 "SQLite default")

## Context
The product spec suggests SQLite with a Postgres option. The ledger, however, is
evidence: it must be append-only, inspectable without our software, and
verifiable by a third party.

## Decision
The ledger of record is `ledger.jsonl`: one canonical JSON entry per line,
appended and fsynced by a single writer under an advisory lock.

## Why
- A reviewer can read it with any text tool and verify it with a 50-line script.
- Append-only is the natural mode of a file; a database invites UPDATE.
- No CGO / third-party driver (ADR-0001).
- Packs ship a byte-exact segment of the same file.

## Consequences
- Queries (packs, alerts) scan the file. At ~400 bytes/entry, 1M entries is
  ~400 MB and scans in seconds — within the "pack in < 5 min for 1M entries" target.
- If we later need fast queries, a SQLite/Postgres *index* can be rebuilt from
  the JSONL at any time; it is never the source of truth.
