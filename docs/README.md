# Feature guides

One page per part of DBCanvas. To install and configure it, see
[Configuration & commands](CONFIGURATION.md).

> [!IMPORTANT]
> **DBCanvas is for testing and learning, not production.** Everything it deploys is disposable.

## Start here

| Guide | What it covers |
| --- | --- |
| [**Getting started**](GETTING_STARTED.md) | Install, the admin account, your first stack, connecting to it, and fixing a failed deploy. |

## Build and run databases

| Guide | What it covers |
| --- | --- |
| [Stacks](STACKS.md) | The canvas, every node and cluster type, deploying, TTLs, node panels, terminals and files. |
| [Operator Backups](OPERATOR_BACKUPS.md) | Backups and restores for a Percona operator on Kubernetes. |
| [All in One](ALL_IN_ONE.md) | Many database instances in one node. |

## Load and query them

| Guide | What it covers |
| --- | --- |
| [Data Generator](DATA_GENERATOR.md) | Fill tables with realistic data. |
| [Query Runner](QUERY_RUNNER.md) | Run SQL on several nodes in parallel. |
| [Database Explorer](DATABASE_EXPLORER.md) | Browse and query databases, including PMM's internal ones. |
| [Benchmark](BENCHMARK.md) | Run workloads and measure throughput and latency. |

## Diagnose them

| Guide | What it covers |
| --- | --- |
| [Packet Inspector](PACKET_INSPECTOR.md) | Capture traffic on a node and decode the database protocol. |
| [Log Summary](LOG_SUMMARY.md) | Several nodes' logs on one timeline. |
| [Stalk Summary](STALK_SUMMARY.md) | Charts and recommendations from a pt-stalk capture. |
| [FTDC Summary](FTDC_SUMMARY.md) | MongoDB's diagnostic data. |
| [Kubernetes States](KUBERNETES_STATES.md) | A live board of every object in a Kubernetes cluster. |
| [Operator Summary](OPERATOR_SUMMARY.md) | Read a pt-k8s-debug-collector dump. |
| [Operator Debugger](OPERATOR_DEBUGGER.md) | Set breakpoints in a running Kubernetes operator. |
| [Core Dump Analyzer](CORE_DUMP_ANALYZER.md) | Read a `mysqld` core dump. |

## Work together

| Guide | What it covers |
| --- | --- |
| [Kanban](KANBAN.md) | Private or shared boards of cards. |
| [Shared Sessions](SHARED_SESSIONS.md) | Share a live session through a link. |

## Learn

| Guide | What it covers |
| --- | --- |
| [Sample Client Code](SAMPLE_CODE.md) | Generate and run client code for a node. |

## Automate

| Guide | What it covers |
| --- | --- |
| [HTTP API](API.md) | Tokens, scopes, status codes and the OpenAPI document. Read this first. |
| [API & CLI reference](API_REFERENCE.md) | Every feature's endpoints and the matching `dbcanvas-cli` command. |
| [`dbcanvas-cli`](CLI.md) | Drive DBCanvas from a terminal or CI. |

For contributors: [Architecture](ARCHITECTURE.md).
