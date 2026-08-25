# Feature guides

How to use each part of DBCanvas. For installing and configuring it, see
[Configuration & commands](CONFIGURATION.md); for how it is built,
[Architecture](ARCHITECTURE.md).

## Build and run databases

| Guide | What it covers |
| --- | --- |
| [Stacks](STACKS.md) | The canvas, every node and cluster type, deploying, TTLs, node panels, web terminals, drag-and-drop file copy, the file manager, and the Docker/hybrid backends. |
| [All in One](ALL_IN_ONE.md) | Many database instances inside a single node — versions and engines side by side. |

## Load and exercise them

| Guide | What it covers |
| --- | --- |
| [Data Generator](DATA_GENERATOR.md) | Realistic test data, at the scale it takes to see a problem. |
| [Query Runner](QUERY_RUNNER.md) | Parallel SQL across nodes, gated on the processlist. |
| [Benchmark](BENCHMARK.md) | OLTP, OLAP, read-write and read-only workloads with throughput and latency. |

## Find out what happened

| Guide | What it covers |
| --- | --- |
| [Packet Inspector](PACKET_INSPECTOR.md) | Capture on a node, decode MySQL, PostgreSQL, MongoDB and Valkey off the wire. |
| [Log Summary](LOG_SUMMARY.md) | Several nodes' logs on one timeline, classified into the good, the warning and the bad. |
| [Stalk Summary](STALK_SUMMARY.md) | Charts from a pt-stalk capture, and which variables to change. |
| [FTDC Summary](FTDC_SUMMARY.md) | MongoDB's diagnostic data — the black box every mongod already writes. |
| [Operator Debugger](OPERATOR_DEBUGGER.md) | Step through the Kubernetes operator itself — breakpoints, call stack and variables, with no IDE. |
| [Core Dump Analyzer](CORE_DUMP_ANALYZER.md) | Read a crashed server's `mysqld` core dump — threads, stack and arguments — without touching the server. |

## Learn on it

| Guide | What it covers |
| --- | --- |
| [Labs](LABS.md) | Hands-on scenarios on a disposable stack, graded against the real cluster. |

---

Screenshots used by these guides live in [`screenshots/`](screenshots).
