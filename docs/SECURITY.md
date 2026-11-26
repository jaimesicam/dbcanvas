# Security model

What DBCanvas can do to the machine it runs on, who can make it do that, and what limits it.
DBCanvas is for testing and learning, not production. Treat it as part of the Docker host it
drives.

## What dbcanvas can do

DBCanvas drives Docker through its socket, and the nodes it deploys are **privileged
containers**. Whoever can deploy a stack can therefore do on the Docker host whatever a privileged
container can, which in effect is root on that host. The same is true of anyone who can use the
Docker socket directly, so this is no more than your user already has when it can run `docker`.

What matters is who else can make your dbcanvas deploy something. The rest of this page is how
that is limited to you, the programs you give a token to, and the guests you invite.

## Who can reach it

**One owner.** dbcanvas runs as your user, keeps its data in `~/.dbcanvas` (readable by your user
only), and whoever runs it owns it. There are no accounts or passwords.

**The control socket.** Commands talk to the service over `~/.dbcanvas/dbcanvas.sock`, which only
your user can open. Status, stop, start links and the node passwords are given on this socket
only.

**The browser port.** The service listens on `127.0.0.1` only (port 8080 by default). Every user
and every process on the computer can connect to 127.0.0.1, so the port answers nothing without
one of these credentials:

| Credential | Where it comes from |
| --- | --- |
| The owner's browser session | A one-time start link from `dbcanvas` or `dbcanvas open`. It works once, for 3 minutes, and sets a session cookie. A link found later in a browser's history signs no one in. |
| An API token | `dbcanvas token create`, or the **API** page. Sent as `Authorization: Bearer …`. |
| A guest's cookie | Joining a shared session you started. |
| A live join link | The link of a shared session that is still running, for a guest who has not joined yet. |

Without one, only the start link, the licence texts under `/licenses/` and the web app's static
files answer (the same code that is in the binary); everything else is a 401 that says how to get
in and nothing about what is running.

**Requests from web pages.** A web page anywhere can make a browser send requests to 127.0.0.1,
through a DNS name that resolves to it or as a cross-site form or fetch. The port answers only
requests addressed to a loopback name or to `publicUrl`, and refuses a write or a websocket whose
`Origin` is another site.

**Other machines.** To use dbcanvas from another machine, forward the port over SSH and sign in
with `dbcanvas open --print`:

```sh
ssh -N -L 8080:127.0.0.1:8080 you@lab-host.example.com
```

A reverse proxy in front of the port works the same way: set `publicUrl` to its address. Either
way, the proxy or tunnel decides who can reach the port, and the credentials above still apply.

## API tokens

- **Scope.** `read` covers every `GET` and the requests that only compute something, except a
  node's terminal, the Operator Debugger and gdb, which hand over a shell or a debugger and need
  `write`; `write` covers everything the browser can do with stacks; `admin` also system
  settings, image builds and the doctor.
- **Limits.** A token can be limited to the stacks it creates (`--own-stacks`) and/or the stacks
  carrying given labels (`--label tool=ci`). Such a token can use only those stacks, list
  (filtered to them), create stacks and read the catalogues; every other endpoint answers 403.
  Stacks it creates carry its labels, and it cannot remove them. `--max-ttl` caps how long its
  stacks may live, and `--deny` names node types it may not deploy.
- **Expiry.** 30 days by default (`--days`), capped by **Settings → API token lifetime**.
  `--days 0` creates a token that never expires.
- **Use.** Give each program the narrowest token that works, and revoke it with
  `dbcanvas token rm` when it is done.

No token, whatever its scope, can create another token or start a shared session: those need
your browser session.

## Shared sessions

A shared session gives guests a link to follow, and optionally drive, what you see. It is off
until you turn it on in **Settings → Shared sessions**, each session ends after the time you
give it, and you can end it at any time. **Hide secrets** keeps passwords out of what guests see.
See [Shared sessions](SHARED_SESSIONS.md).

## Node passwords

The accounts DBCanvas creates on nodes get passwords generated on the first run and kept
encrypted in the database; `dbcanvas passwords` shows them. They are your lab's passwords, not a
published default. See [Configuration](CONFIGURATION.md#node-passwords).

## Secrets at rest

Passwords and stack secrets in the database are encrypted with the key in
`~/.dbcanvas/dbcanvas-encryption.key`. Back up the key with the database, or somewhere safer:
without it the database cannot be read. `dbcanvas rotate-key` re-encrypts everything under a new
key. See [Configuration](CONFIGURATION.md#the-encryption-key).

## Exported ports

Ports a node exports to the host are published on `127.0.0.1` (`CONTAINER_BIND_IP`). Setting
`CONTAINER_BIND_IP=0.0.0.0` publishes them on every interface, where anyone on the network can
reach the databases with the node passwords.

## What is not supported

**A Docker socket proxy.** A proxy that filters the Docker API does not reduce what dbcanvas can
do: it creates privileged containers, and it mounts the socket into helper containers for image
builds and K3D clusters. What protects the host is limiting who can reach dbcanvas, which is what
the credentials above do.
