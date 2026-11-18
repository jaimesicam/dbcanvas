# The `dbcanvas` command

`dbcanvas` is the whole program: the web app, the deploy engine and the command line, in one
binary. This page covers every command. For the browser, start with
[Getting started](GETTING_STARTED.md); for settings, [Configuration](CONFIGURATION.md).

```text
dbcanvas                      open dbcanvas in the browser, signed in
dbcanvas open [--print]       the same; --print prints the one-time sign-in link instead
dbcanvas ls [--json]          list your stacks
dbcanvas export STACK [-o FILE]
dbcanvas up -f FILE [--platform amd64|arm64] [--dry-run] [--wait] [--timeout 1h] [--json]
dbcanvas up NAME --node SPEC... [--ttl 8h] [--label k=v]... [--platform …] [--dry-run]
            [--wait] [--timeout 1h] [--json]
dbcanvas down STACK           remove a stack's containers, keep its design
dbcanvas rm STACK             delete a stack and everything it deployed
dbcanvas exec STACK NODE [--user USER] [--timeout 10m] [-i] [--json] -- CMD [ARG...]
dbcanvas cp STACK NODE:/path ./file | dbcanvas cp STACK ./file NODE:/path
dbcanvas token create NAME [--scope read|write|admin] [--days 30]
               [--own-stacks] [--label k=v]... [--max-ttl 8h] [--deny TYPE,...]
dbcanvas token ls | token rm ID
dbcanvas passwords [NAME]
dbcanvas kinds [--json]       the compose kinds and their options
dbcanvas commands [--json]    every command with its arguments and flags
dbcanvas status [--json]      whether the service runs, and what keeps it up
dbcanvas stop                 stop the service
dbcanvas versions [update|reset]
dbcanvas doctor
dbcanvas network check
dbcanvas rotate-key
dbcanvas version
```

`STACK` is a stack's name or its ID, as `dbcanvas ls` shows them. `NODE` is a node's label
(`ps-01`) or its ID. `dbcanvas COMMAND -h` prints one command's flags.

## The service

The web app and the deploy engine run as a background service of your own user. There is
nothing to install or enable:

- Any command that needs the service starts it when it is not running, in the background,
  and returns the prompt at once. `dbcanvas`, `open`, `ls`, `export`, `up`, `down`, `rm`,
  `token` and `passwords` need it.
- It stops by itself when no stack has containers and nothing has used it for 10 minutes
  (`DBCANVAS_IDLE_GRACE` changes the wait). A stack that is deployed keeps it running until
  the stack's TTL ends.
- `dbcanvas stop` stops it now. Stacks keep running; the next command starts it again.
- After a reboot it is not running until a command needs it.
- When the `dbcanvas` file is replaced by a newer build, the service switches to it once no
  deploy is running and no browser tab is open.

`dbcanvas status` says whether it runs, on which port, and what keeps it up (deployed
containers, a running deploy, open connections):

```console
$ dbcanvas status
dbcanvas 0.1.0 is running (pid 4242, since 2026-11-17T15:58:35Z) at http://127.0.0.1:8080/
Instance dbcanvas-alex, data in /home/alex/.dbcanvas
Docker runs on amd64; nodes default to amd64, Kubernetes nodes run on amd64
Kept up by: 3 node(s) with containers
```

The service is reached on a socket only your user can open, `~/.dbcanvas/dbcanvas.sock`. Its
log is `~/.dbcanvas/dbcanvas.log`.

## Signing in: `dbcanvas` and `dbcanvas open`

`dbcanvas` (or `dbcanvas open`) opens your browser on DBCanvas, signed in. It does this with a
one-time start link that works once, within a minute, and sets a session cookie in that
browser.

On a server with no browser (Linux with no display, or any SSH session), `dbcanvas` opens
nothing: it prints the SSH tunnel to make from your own computer, with this machine's port, and
the link to open there. `dbcanvas open --print` prints just a new link, the 127.0.0.1 one first:

```sh
# on your laptop: the same port number on both ends
ssh -N -L 8080:127.0.0.1:8080 alex@lab-host.example.com
# on lab-host
dbcanvas open --print
# open the printed link on the laptop, within a minute
```

DBCanvas answers only on `127.0.0.1:<its port>`, so forward the same port number: a tunnel to
another local port is refused. When you reach it through a tunnel or reverse proxy of your own
instead, set `publicUrl` in `~/.dbcanvas/config.yaml`; the link is then printed on that address
too.

The port listens on 127.0.0.1 only and answers nothing without a credential. See
[Security](SECURITY.md).

## Stacks

### `dbcanvas ls`

```console
$ dbcanvas ls
ID  NAME      STATUS    TTL  EXPIRES
1   pxc-lab   deployed  8h   2026-11-18T02:15:16Z
```

### `dbcanvas export STACK [-o FILE]`

Writes the stack as a stack file to standard output, or to `FILE`. The file carries the whole
canvas design, so applying it again gives the same stack.

### `dbcanvas up`

Creates a stack, or updates it when a stack of that name exists, then deploys it. The stack
comes from a [stack file](#stack-files) (`-f FILE`, or `-f -` for standard input), or from
[compose entries](#compose-entries) on the command line:

```sh
dbcanvas up pxc-lab --node 'pxc:3,version=8.4,monitor' --node pmm --ttl 8h --label team=qa --wait
```

which is the same as a stack file with that `name`, `ttl`, `labels` and `nodes` list. Without
`--ttl` or `--label`, a stack that exists keeps its own.

| Flag | |
| --- | --- |
| `--dry-run` | Show what would be added, changed and removed, and change nothing. |
| `--wait` | Wait until every node is running, or one fails. The exit status is non-zero on a failure. |
| `--timeout 1h` | How long `--wait` waits. |
| `--platform amd64\|arm64` | The architecture of the database nodes that do not choose their own. Overrides the file's `platform:`. |
| `--node SPEC` | A compose entry, repeatable. With `NAME`, instead of `-f`. |
| `--ttl 8h` | The stack's lifetime: `2h`, `4h`, `8h`, `24h`, `2w` or `30d`. With `NAME`. |
| `--label k=v` | A label on the stack, repeatable. With `NAME`. |
| `--json` | Print the plan, whether the deploy started, and with `--wait` each node's final state and error, as one JSON object. |

Without `--wait`, `up` returns once the deploy has started; `dbcanvas ls` and the browser show
its progress.

```console
$ dbcanvas up -f pxc-lab.yaml --dry-run
Would create pxc-lab
  + node  intranet-01 (arm64)
  + node  pxc-01 (arm64)
  + node  pxc-02 (arm64)
  + node  pxc-03 (arm64)
  + node  pmm-01 (arm64)
  + frame pxc-cluster-01 (arm64)
```

Applying a file to a stack that exists is declarative: the stack's design becomes the file's,
a node no longer in it loses its container, and the deploy builds what is not running. A
running node whose settings changed keeps running as it is, and the plan says so.

### `dbcanvas down STACK` and `dbcanvas rm STACK`

`down` removes the stack's containers and networks and keeps its design, to deploy again
later. `rm` removes everything, the design included.

## Working on nodes

### `dbcanvas exec STACK NODE -- CMD [ARG...]`

Runs one command in a node, without a terminal, and returns when it ends. The command's
standard output and standard error are dbcanvas's, and dbcanvas exits with the command's exit
status, so it works in scripts like a local command:

```console
$ dbcanvas exec pxc-lab pxc-01 -- mysql -N -e 'select @@version'
8.4.6-6.1
$ dbcanvas exec pxc-lab pxc-01 -- test -f /var/lib/mysql/auto.cnf; echo $?
0
```

The command runs as the node's default user (root on every node image) in a login shell, so
`PATH` is the one the node's console has. The arguments are passed as they are, with no shell in
between: write `-- sh -c '…'` for pipes and redirections. Without `--`, everything after `NODE`
is the command, and dbcanvas's own flags must come before `STACK`.

| Flag | |
| --- | --- |
| `--user USER` | Run as this user. |
| `--timeout 10m` | Stop the command after this long, at most `1h`. A stopped command exits with 124. |
| `-i` | Pass standard input to the command, up to 16 MiB: `dbcanvas exec -i lab ps-01 -- mysql < dump.sql`. |
| `--json` | Print `exitCode`, `stdout`, `stderr`, `timedOut` and `durationMs` as one object instead (binary output comes base64-encoded in `stdoutBase64`). The exit status is still the command's. |

Output is limited in size; for a large result, write it to a file on the node and copy it with
`dbcanvas cp`.

### `dbcanvas cp`

Copies one regular file between this machine and a node:

```sh
dbcanvas cp pxc-lab pxc-01:/etc/my.cnf ./my.cnf       # from the node
dbcanvas cp pxc-lab ./my.cnf pxc-01:/etc/my.cnf       # to the node
dbcanvas cp pxc-lab ./dump.sql pxc-01:/root/          # into a directory, keeping the name
```

The node side is written `NODE:/absolute/path`. Copying to a node, the destination is a file
name, or a directory that exists (with or without a trailing `/`). Copying from a node into a
local directory keeps the file's name. Directories are not copied: archive one on the node with
`dbcanvas exec … -- tar czf /tmp/x.tgz …` and copy the archive. The browser's file manager copies
directories and between nodes.

## Scripting

- `--json` on `ls`, `status`, `up` and `exec` prints JSON instead of text. `dbcanvas status
  --json` answers `{"running": false}` without starting the service.
- `dbcanvas kinds` lists the compose kinds with their options and what they need, from the
  binary itself; `--json` prints the same as `GET /api/stacks/compose/kinds`.
- `dbcanvas commands --json` lists every command, its arguments, its flags (name, kind, default)
  and whether it starts the service. It is built from the same table as the help and the flag
  parsing, so it is exactly what this build accepts.
- The exit status says what happened (below).

## Stack files

A stack file is YAML (JSON is read too):

```yaml
apiVersion: dbcanvas/v1
kind: Stack
name: pxc-lab
ttl: 8h
labels: {team: example}
platform: arm64        # optional: the database nodes' architecture
nodes:
  - pxc:3,version=8.4,os=el9,monitor
  - pmm
```

| Key | |
| --- | --- |
| `name` | The stack's name. Required. |
| `ttl` | How long the stack lives: `2h`, `4h`, `8h`, `24h`, `2w` or `30d`. Every stack expires; extending one never takes it further than 30 days from now. A new stack without one gets the default; an existing stack keeps its own. |
| `labels` | Key/value labels. An empty map clears them. |
| `platform` | `amd64` or `arm64` for every database node that does not choose its own architecture. |
| `nodes` | Compose entries, one per node or cluster (below). |
| `design` | The full canvas document, as `dbcanvas export` writes it. Give `nodes` or `design`, not both. |

### Compose entries

A compose entry is a kind, an optional count, and options:

```yaml
nodes:
  - ps:1,version=8.0,os=el8,ldap
  - pxc:3,version=8.4,monitor
  - patroni:3,name=site-a
  - patroni:3,name=site-b,standbyOf=site-a
  - pmm
```

The same entry can be written as a map (`{kind: pxc, count: 3, version: "8.4"}`). The Intranet
node every stack needs is added for you. A short version such as `8.4` resolves to the newest
build of that line in the version catalogue, and `up --dry-run` shows what it resolved to.

`dbcanvas kinds` lists them all with their options. The kinds include `ps`, `pxc`, `ps-repl`, `mysql`, `mysql-repl`, `innodb`, `mariadb`,
`mariadb-galera`, `pg`, `pg-pgdg`, `patroni`, `pg-stream`, `repmgr`, `spock`, `psmdb`, `psmrs`,
`mdb`, `mdb-rs`, `valkey`, `valkey-cluster`, `valkey-sentinel`, `proxysql`, `haproxy`,
`pgbouncer`, `orchestrator`, `pmm`, `seaweedfs`, `keycloak`, `openbao`, `sambaad`, `vnc` and
the simulators.

Any field the design catalogue lists can be set in a one-line entry too: `opt.FIELD=VALUE`
sets it on the node (or a cluster's frame), and `member.FIELD=VALUE` on every member of a
cluster. That is how a K3D cluster is composed, for example a Percona Operator for MySQL
cluster on two k3s nodes:

```sh
dbcanvas up k8s-lab --node 'k3d:2,opt.k3dOperator=pxc,opt.k3dOperatorVer=1.20.0' --ttl 8h --wait
dbcanvas exec k8s-lab k3d-1 -- kubectl get pods -A
```

`dbcanvas kinds` lists the K3D settings that matter most; the design catalogue lists them all.
Kubeadm, All in One, the Stock Market Sim and core-dump hosts are not built by compose: start
them from a template in the browser, then `dbcanvas export` the stack and keep the file.

## Tokens: `dbcanvas token`

An API token lets a script, or another program of yours, use the HTTP API (see
[HTTP API](API.md)).

```console
$ dbcanvas token create nightly --scope write --days 30 --max-ttl 8h
Token 2, "nightly" (write scope), expires 2026-12-17T15:59:16Z.
Its secret is shown this once:

  dbc_…
```

| Flag | |
| --- | --- |
| `--scope read` | Every `GET`, and the requests that only compute something (validate, compare, preview). |
| `--scope write` | Everything the browser can do with stacks. The default. |
| `--scope admin` | Also instance settings, tokens and image builds. |
| `--days 30` | Days until it expires; `0` never expires. |
| `--own-stacks` | Reach only the stacks this token creates. |
| `--label k=v` | Reach only stacks with this label, which it puts on every stack it creates. Repeatable. |
| `--max-ttl 8h` | The longest a stack it creates may live. |
| `--deny TYPE,...` | Node types it may not deploy. |

`dbcanvas token ls` lists tokens (never their secrets), and `dbcanvas token rm ID` revokes
one. The **API** page in the browser does the same.

## Node passwords: `dbcanvas passwords`

The accounts DBCanvas creates on nodes (database root and application users, PMM's admin,
the VNC desktop, Keycloak's sample users and so on) take their passwords from settings such
as `APP_PASSWORD` and `PMM_ADMIN_PASSWORD`. They are generated on the first run and kept
encrypted in the database.

```console
$ dbcanvas passwords
$ dbcanvas passwords PMM_ADMIN_PASSWORD
```

A value set under `env:` in `config.yaml` takes precedence over the generated one. A node keeps
the password it was deployed with. See [Configuration](CONFIGURATION.md#node-passwords).

## Maintenance

### `dbcanvas versions [update|reset]`

DBCanvas offers the database versions its version catalogue lists, one catalogue per
architecture. The catalogue is built into the binary. `dbcanvas versions` shows which one is in
use; `dbcanvas versions update` downloads the newest from the project's repository into the data
folder, where it is read ahead of the built-in one; `dbcanvas versions reset` deletes the
download. Nothing is downloaded unless you run `update`. A running service uses an updated
catalogue from its next deploy.

### `dbcanvas doctor`

Checks Docker and access to its socket, the architecture, the base images, both catalogues,
the data folder's permissions and free space, the encryption key, the node passwords (a
warning for one set to a published example value), stack networks, k3d, the bundled files
and the service, and says how to fix each problem:

```console
$ dbcanvas doctor
ok    docker             answers on /var/run/docker.sock
ok    architecture       Docker runs on amd64; nodes default to amd64, Kubernetes nodes run on amd64
ok    stack networks     the next stack gets 10.125.226.0/24 (dbcanvas network check shows why)
...
```

It exits non-zero when a check fails.

### `dbcanvas network check`

Shows where the next stack network would go and every range ruled out: Docker's networks,
Docker's default address pools, this host's interfaces and routes, and the Kubernetes ranges.
It needs no running service. Run it on a shared host before deploying there, and set
`networkRange` in `config.yaml` if the default search space is not yours to use. See
[Configuration](CONFIGURATION.md#stack-networks).

### `dbcanvas rotate-key`

Re-encrypts the secrets in the database under a new key. It stops the service first; stacks
keep running, and the next command starts the service again. The old key is kept beside the new one as
`<key>.old`: back up the new key, then delete the old one.

### `dbcanvas version`

Prints the version.

## Exit status

| Status | |
| --- | --- |
| `0` | Success. |
| `1` | The command failed; the reason is on standard error. `doctor` exits 1 when a check fails. |
| `2` | A usage error: an unknown command or flag, or missing arguments. |
| `3` | `up --wait` saw a node fail, or the deploy did not finish within `--timeout`. |
| any | `exec` exits with the command's own status (124 when `--timeout` stopped it). |
