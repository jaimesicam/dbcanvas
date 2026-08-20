# Labs (experimental)
A catalog of **95 hands-on scenarios** — 27 for **Patroni** (PostgreSQL HA), 30 for **PS MongoDB**
(10 each for standalone, replica set, and sharded), 23 for **Valkey** (15 standalone, 8
cluster), and 15 for the **MySQL family** (6 MySQL Replication, 6 PXC, 3 HAProxy+PXC) — grouped
by category (Failover & Elections, Sharding & Routing, Security & Access
Control, Backup & Recovery, …) and difficulty. Starting a lab provisions a real, disposable stack
through the same design-JSON + deploy pipeline **Database Stacks** uses; each step's **Check
Work** button inspects that stack's actual live state — real `rs.status()`, `config.chunks`,
`patronictl`/`kubectl` output, replication lag, and so on — it never grades what you typed.

> **This content is AI-generated and experimental — that's why it's labeled "(experimental)" in
> the app itself.** The lecture notes, step instructions, hints, and every Check Work function were
> written by an LLM and spot-checked against real deployed stacks during development, not reviewed
> line by line by a subject-matter expert. Treat it as a fast-drafted starting point, not a vetted
> curriculum: technical claims, command syntax, and pass/fail conditions can be wrong. **Verify
> anything before teaching from it, using it for certification prep, or relying on it
> operationally.**

![The Labs (experimental) catalog, grouped by database, technology and category](screenshots/labs.png)

> *Patroni's "Leadership & Failover" category: each card names its scenario, difficulty and time
> limit, with lecture notes and a **Start Lab** button that deploys a real 3-node cluster behind
> HAProxy — Check Work grades the cluster's actual state, not anything typed into a form.*
---

See also: [Stacks](STACKS.md) · [Feature guides](README.md)
