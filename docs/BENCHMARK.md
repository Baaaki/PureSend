# PureSend — Performance Measurements

[Türkçe](BENCHMARK_TR.md)

Every number in this document was measured and is given together with its command; it can be run again on the same machine. What has not been measured is listed separately at the end.

> **Hardware and environment:** AMD Ryzen 7 5700X (8 cores / 16 threads), 32 GB RAM, NVMe SSD, Linux 6.8, Go 1.27.1, on a desktop with other programs running. Measured code: v2.0.7 (`7024fdf`), on 2026-10-01.

---

## 1. Summary

| Measurement | Result | Source |
| :--- | :---: | :--- |
| End-to-end transfer speed (loopback) | **270–440 MB/s** | `make bench-e2e`, 256 MiB – 8 GiB |
| Peak memory (RSS), sender / receiver | **36–39 MB / 33–36 MB** | same measurement, 256 MiB – 8 GiB |
| Handshake, CPU work of both sides | **~0.65 ms** | `BenchmarkHandshake` |
| Chunk compression / decompression | **~800 MB/s / ~225 MB/s** | `BenchmarkCompressChunk`, `BenchmarkDecompressChunk` |

**How to read it:** Loopback has no wire speed of its own. The end-to-end measurement therefore shows not the network but the ceiling the software sets: the encrypted libp2p connection, SHA-256 on both ends, writing to disk and the protocol itself. Even the slowest run (269 MB/s) is more than twice the practical ceiling of gigabit Ethernet (~118 MB/s), so on this hardware the bottleneck on a local network is the network itself. On a slower CPU or disk this ceiling drops. The spread between runs is wide (section 2), which is why a range is given and not a single figure.

---

## 2. End-to-end measurement (`make bench-e2e`)

[`scripts/bench-e2e.sh`](../scripts/bench-e2e.sh) measures the program the way a user runs it:

1. It starts a local rendezvous server (`puresend-server`, TCP, loopback).
2. It generates a file of the requested size from `/dev/urandom`. Random data does not compress, so every byte crosses the connection.
3. It starts the sender and waits for it to finish reading and hashing the files (`files ready`), so preparation time does not enter the measurement.
4. It starts the receiver and times it: startup, room lookup, connection, handshake, transfer and verification included.
5. It checks that the connection was established directly and that the SHA-256 digests of the two files are identical.
6. It reports the peak memory of the two processes with GNU `time`.

Results (each row is a separate run):

| Data | Receiver time | Speed | Peak RSS, sender | Peak RSS, receiver |
| :---: | :---: | :---: | :---: | :---: |
| 256 MiB | 0.99 s | 271 MB/s | 36 MB | 33 MB |
| 2 GiB | 6.70 s | 320 MB/s | 38 MB | 34 MB |
| 2 GiB | 5.72 s | 375 MB/s | 39 MB | 35 MB |
| 2 GiB | 6.53 s | 329 MB/s | 38 MB | 35 MB |
| 8 GiB | 31.93 s | 269 MB/s | 38 MB | 36 MB |

- **Memory does not grow with file size.** With a file 32 times larger, peak RSS moves by a few MB. Files are read and written in 32 KB chunks and are never held in memory whole.
- **The spread between runs is large, and it is the machine, not the code.** A second batch the same day gave 272–437 MB/s on v2.0.7 (2 GiB: 416, 401, 309; 8 GiB: 437; 256 MiB: 272). Binaries built from v2.0.0 (`27ff753`) and run in alternation with the v2.0.7 ones gave 302–458 MB/s, so there is no regression between the two versions. We did not isolate the cause of the spread; other processes and the state of the page cache and disk write-back are the suspects.
- **At 256 MiB the speed is usually lower**, because the fixed part of the time (process startup, server connection, handshake) is proportionally larger in a short transfer.
- The receiver writes the data into the operating system's page cache. The speed at which it actually reaches the disk is a separate matter.

```bash
make bench-e2e                       # 2 GiB
FT_BENCH_SIZE_MB=8192 make bench-e2e # 8 GiB
```

---

## 3. Micro-benchmarks

These measure a single function. Their real job is to catch regressions; the speed a user feels is shown by the end-to-end measurement.

```bash
make bench   # go test -run '^$' -bench . -benchmem ./...
```

Range of three runs:

| Benchmark | Time | Speed | Memory | Allocations |
| :--- | :---: | :---: | :---: | :---: |
| `BenchmarkCompressChunk` | 39–40 µs | 792–812 MB/s | ~58 B | 1 |
| `BenchmarkDecompressChunk` | 140–146 µs | 219–228 MB/s | ~125 B | 3 |
| `BenchmarkHandshake` | 0.63–0.68 ms | — | ~30 KB | 373–374 |
| `BenchmarkSafeJoin` | 621–808 ns | — | 240 B | 5 |
| `BenchmarkValidDigest` | 25–26 ns | — | 0 B | 0 |
| `BenchmarkClean_CleanText` | 705–803 ns | — | 120 B | 4 |
| `BenchmarkClean_UnsafeText` | 384–414 ns | — | 56 B | 3 |

- **Compression** is measured on a text-like 32 KB chunk. It uses `flate.HuffmanOnly`, because this mode, which does not search for matches, runs above line speed. A chunk that does not shrink (JPEG, MP4, ZIP and the like) is sent as it is, so nothing grows on the wire for incompressible data.
- **The handshake** measures the PAKE and mutual confirmation steps of both sides together over an in-memory pipe. On a real connection one or two network round trips come on top, and they decide the time.
- **The security checks** (`SafeJoin`, `Clean`) are below a microsecond and run once per file, not per byte. Their effect on transfer speed is too small to measure.

---

## 4. Not Measured Yet

- **LAN speed over a real network:** The loopback ceiling is 270–440 MB/s. It has not been measured between two machines over a gigabit or 2.5G link.
- **Speed over the internet and the hole-punching success rate:** How often DCUtR manages a direct connection across different home, mobile (CGNAT) and corporate network pairs has not been measured.
- **Speed over the relay:** The relay fallback is tested for correctness on every commit with [`test/relay`](../test/relay/netns-relay-test.sh), but its speed is not measured. The production server limits each relayed connection in data and time (`-relay-data`, `-relay-duration`).
