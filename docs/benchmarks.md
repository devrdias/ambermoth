# Benchmarks

Two targets from the roadmap: proxy overhead under 20 ms at p50 on non-streaming calls, and a pack build
under 5 minutes for a ledger of 1M entries. The benchmarks are not part of `make check` or CI.

```
make bench       # proxy (count 5), then pack at 10k and 100k entries
make bench-1m    # pack at 1M entries
```

Single sizes and filters work as usual, for example
`go test ./internal/proxy -run '^$' -bench 'Proxied/1KB' -benchmem`. For pack, `AMBERMOTH_BENCH_SIZES=250000,500000`
replaces the default sizes and `AMBERMOTH_BENCH_1M=1` adds 1M. A size is skipped when the temp directory has less
than 3000 bytes of free disk per entry. A ledger of 1M entries is 847 MB.

## Proxy overhead

`internal/proxy/bench_test.go`. The upstream is an in-process `httptest.Server` that answers at once with a chat
completion of about 1.5 KB with a `usage` block. `BenchmarkDirect` sends the request straight to it and
`BenchmarkProxied` sends it through the proxy, with a real ledger in a temp directory and the writer set up as
the CLI does it: queue of 10000, flush every 200 ms, one fsync per batch. Both use the same keep-alive client.
Request bodies are 1, 16 and 256 KB, sent one after the other and with `b.RunParallel`.

Per call latency is recorded for every call and reported as `p50-ms`, `p95-ms` and `p99-ms`. The overhead is
the proxied figure minus the direct one. `failed` counts calls that returned an error and `dropped` counts calls
the writer did not record (always 0 here). `ns/op` and `allocs/op` cover the client, the upstream and the proxy,
which share the process, so compare proxied with direct.

`BenchmarkStreamDirect` and `BenchmarkStreamProxied` use an SSE response of 52 events, flushed one by one, and
add `ttfb-*` metrics, the time to the first byte.

Two tests run with `go test`. `TestStreamIsNotBuffered` has the upstream hold back the stream after the first
chunk and fails if the client does not get that chunk. `TestOverheadStaysUnderTarget` fails if the median
overhead over 200 sequential 1 KB calls is over 20 ms. It takes 0.1 s and measures about 0.1 ms, so only a gross
regression trips it, and it is on in CI. It is skipped with `-race` and `-short`.

## Pack build and verification

`internal/pack/bench_test.go`. The generator writes a ledger through `Store.Append`, one batch per chunk, at
26k to 28k entries/s (100k: 26,374; 1M: 28,109 in the Verify run). Entries are calls with the SPEC 4.2 fields over two providers, five models, eight purposes
and three keys, plus two gaps, amendments, key enrolments and a rotation. Each chunk ends with an anchor. Tokens
are real `tsatest` tokens, one openssl run each, so a ledger gets 50 anchors whatever its size (at 1M, one
anchor per 20000 entries). The ledger passes `ledger.Verify` with pinned roots, and a normal test checks that
and that packs built from it verify. The ledger covers 20 whole days ending before today.

- `BenchmarkBuild`: `pack.Build` over the whole ledger, and over days 9 and 10 (the middle 10%).
- `BenchmarkVerifyPack`: `pack.VerifyPack` on those two packs.
- `BenchmarkVerify`: stream the ledger file through `ledger.VerifyReader`, as `ambermoth verify` does.

`entries/s` is entries in the ledger for Build and Verify, and entries in the pack segment for VerifyPack.
`peak-heap-MB` is the highest heap in use, sampled every 10 ms, above the heap at the start. It includes garbage
the collector has not freed yet, so it is closer to the resident size than to the live set.

## Machine

| | |
|---|---|
| CPU | Intel Xeon @ 2.10 GHz, 2 cores (`nproc` 2) |
| RAM | 8 GB, no swap; the sandbox cgroup killed a process at 6.2 GB resident |
| Disk | virtio block device (`/dev/vda`), 252 GB, type not known |
| OS | Ubuntu 24.04.5, kernel 6.18 |
| Go | 1.24.7 linux/amd64 |
| Date | 2026-11-27 |
| Commit | 4fba8e0 (benchmark code; this file was added on top) |

## Results: proxy

Median of 5 runs. Times in ms. With 2 cores, the parallel runs use 2 goroutines.

| Body | Mode | Direct p50 / p95 / p99 | Proxied p50 / p95 / p99 | Overhead p50 / p95 / p99 |
|---|---|---|---|---|
| 1 KB | sequential | 0.033 / 0.092 / 0.167 | 0.153 / 0.743 / 1.57 | 0.12 / 0.65 / 1.40 |
| 1 KB | parallel | 0.031 / 0.068 / 0.265 | 0.188 / 1.21 / 2.34 | 0.16 / 1.14 / 2.08 |
| 16 KB | sequential | 0.037 / 0.128 / 0.315 | 0.345 / 1.04 / 1.79 | 0.31 / 0.92 / 1.48 |
| 16 KB | parallel | 0.046 / 0.129 / 0.579 | 0.339 / 1.48 / 2.92 | 0.29 / 1.35 / 2.34 |
| 256 KB | sequential | 0.122 / 0.300 / 0.566 | 2.99 / 4.32 / 5.82 | 2.87 / 4.02 / 5.25 |
| 256 KB | parallel | 0.143 / 0.412 / 0.855 | 3.55 / 6.67 / 8.64 | 3.40 / 6.26 / 7.79 |

Streaming, 1 KB request, 52 events, sequential:

| | Direct p50 / p95 / p99 | Proxied p50 / p95 / p99 |
|---|---|---|
| First byte | 0.150 / 0.257 / 0.386 | 0.437 / 1.20 / 1.86 |
| Total | 0.178 / 0.422 / 0.617 | 0.518 / 1.29 / 1.98 |

Allocations per call: 85 direct and about 810 proxied for 1 KB, 2257 proxied for the stream (87 direct). The
proxy allocates about 77 KB per 1 KB call and 1.05 MB per 256 KB call. No call was dropped by the writer.

Target met on this machine: **yes**. The p50 overhead is 0.12 ms for 1 KB bodies and 3.4 ms at worst for 256 KB
bodies, against 20 ms. Evidence: the table, and `TestOverheadStaysUnderTarget`. The overhead grows with the body
size. A CPU profile of the 256 KB case puts about 70% in `encoding/json` scanning the request body and 6% in
SHA-256. The proxy does not buffer streams: the first byte arrives 0.29 ms after it would direct, and the test
above checks it.

## Results: pack

One run per size (a run of these takes minutes). Time in seconds, peak heap in MB.

| Entries | Operation | Time | Entries/s | Peak heap |
|---|---|---|---|---|
| 10k | Build, whole ledger | 2.06 | 4880 | 115 |
| 10k | Build, middle 10% | 1.38 | 7288 | 76 |
| 10k | VerifyPack, whole | 1.53 | 6558 | 93 |
| 10k | VerifyPack, middle 10% | 0.18 | 6626 | 12 |
| 10k | Verify ledger, before streaming | 1.16 | 8676 | 73 |
| 100k | Build, whole ledger | 23.7 | 4223 | 1044 |
| 100k | Build, middle 10% | 14.1 | 7097 | 744 |
| 100k | VerifyPack, whole | 14.9 | 6725 | 905 |
| 100k | VerifyPack, middle 10% | 1.68 | 7145 | 109 |
| 100k | Verify ledger, before streaming | 12.9 | 7733 | 746 |
| 100k | Verify ledger, streamed | 5.95 | 16821 | 37.0 |
| 250k | Build, whole ledger | 62.8 | 3983 | 2406 |
| 250k | Build, middle 10% | 35.0 | 7151 | 1835 |
| 250k | VerifyPack, whole | 36.1 | 6930 | 2287 |
| 250k | VerifyPack, middle 10% | 4.41 | 6801 | 275 |
| 250k | Verify ledger, before streaming | 31.1 | 8045 | 1820 |
| 500k | Build, whole ledger | 125 | 3993 | 4824 |
| 500k | Build, middle 10% | 77.2 | 6476 | 3850 |
| 500k | VerifyPack, whole | 69.2 | 7228 | 4473 |
| 500k | VerifyPack, middle 10% | 8.33 | 7205 | 558 |
| 500k | Verify ledger, before streaming | 62.3 | 8032 | 3756 |

Notes on how this was run: the 500k Build of the whole ledger comes from an earlier run of the same code that
was cut off after that benchmark, and the 250k and 500k runs used `-benchtime 1x`.

Scaling of Build over the whole ledger: 2.06 s, 23.7 s, 62.8 s and 125 s at 10k, 100k, 250k and 500k. From 100k
on that is a steady 250 µs per entry, so time is linear, not quadratic. Heap was linear too, at about 9.6 KB per
entry for Build and 7.5 KB for Verify. These are the rows marked before streaming: `Build` read the whole ledger
into memory (entries and raw lines), verified it, and then verified the segment again, which is why a whole-ledger
Build cost twice a Verify. `Verify` and `VerifyPack` also held everything. A middle-10% pack saved little on Build,
since the whole ledger was still read and verified first.

### 1M entries

One run each (`-benchtime=1x`) on the machine above, with the pack code at 8e12604 and the Verify change in
this commit. The ledger has 1,000,060 entries and 50 anchors, and is 847 MB on disk. Generating it took 35.6 s.

| Operation | Time | Entries/s | Peak heap (MB) |
|---|---|---|---|
| Build, whole ledger | 107.5 s | 9299 | 887.6 |
| Build, middle 10% | 68.4 s | 14629 | 403.6 |
| VerifyPack, whole | 70.5 s | 14180 | 1224 |
| VerifyPack, middle 10% | 8.7 s | 13781 | 142.5 |
| Verify ledger, streamed | 57.4 s | 17422 | 378.2 |

Target met on this machine: **yes**. Build of the whole 1M ledger took 107.5 s, under the 300 s target. Each
figure is one run, not a median.

## What the numbers point at

These were found by profiling and are not fixed here.

- Canonical JSON is the cost of Build and Verify. In a CPU profile of Verify, `canon.Marshal` is about half of
  the time and `canon.sortedKeys` about 30%, nearly all in `utf16Less`, which converts both keys to UTF-16 on every
  comparison. It is also a third of all bytes allocated. Entries are `map[string]any`, so each is sorted again on
  every hash and every check.
- The ledger is read as a stream now. The highest peak heap of the 1M runs is 1224 MB (VerifyPack, whole ledger).
- Build verifies the segment a second time.
- The proxy scans the request body with `encoding/json` (`checkValid` and `skip`), the main cost at 256 KB, and
  `teeBody.Read` allocates 84% of the bytes allocated in that case.
- Streams allocate about 2200 times per call against 87 direct, around 40 per event.
- Intermittent truncated streams, fixed. With 50 flushed events and sequential calls, about 1 call in 2000 to
  25000 failed with `unexpected EOF`, the proxy logging `use of closed network connection`. The cause is a race in
  Go's `net/http` (1.24, 1.25 and 1.26 have the same code). After copying Content-Length bytes,
  `transferWriter.writeBody` reads the request body once more. If the upstream has already answered, the server has
  closed the incoming body by then, the read fails with `http: invalid Read on closed Body`, and
  `persistConn.writeLoop` closes the upstream connection in the middle of the stream. A plain `httputil.ReverseProxy`
  does the same. `teeBody.Read` now returns EOF again once the body has ended, so that read never reaches the closed
  body. The stream benchmarks still count failed calls in `failed` and go on.
- Not fixed, not decided: a stream cut for any reason is recorded with `client_aborted` true, whoever cut it, and
  with the tokens that had been read, which is 0 if the usage chunk had not arrived.
