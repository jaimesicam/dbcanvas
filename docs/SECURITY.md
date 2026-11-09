# Security model

What DBCanvas can do to the machine it runs on, who can make it do that, and the settings that
limit it. DBCanvas is for testing and learning, not production. Treat an installation as part of
the Docker host it runs on.

## What an account can do

DBCanvas drives the host's Docker daemon through its socket, and the nodes it deploys are
**privileged containers**. So anyone who can deploy a stack can do on the host whatever a
privileged container can do. In effect, that is root on the Docker host. This is true of every
account that can use **Database Stacks**, not only administrators.

Two things follow:

- **Who can reach the sign-in page matters.** Keep `APP_HOST=127.0.0.1` (the default in `.env`)
  unless the people on that network are people you would give a shell on the host. Colleagues can
  reach a loopback installation through an SSH tunnel: `ssh -N -L 8080:127.0.0.1:8080 you@host`.
- **Who has an account matters.** Leave **Open registration** off, so an administrator approves
  every account.

## Guest sign-in

**Continue as guest** (off by default) gives anyone who can open the sign-in page an account with
no password: a guest account of its own, in the **Guests** security group. Each guest's stacks
are its own. An unused guest account is removed with its stacks after the idle period (7 days by
default).

While DBCanvas is reachable beyond the machine (`APP_HOST` is not a loopback address), guest
sign-in has to be confirmed by an administrator in Settings → Guest sign-in. Without that
confirmation the server refuses both turning it on and every guest sign-in. Turning it off signs
every guest out and revokes their tokens, unless **Keep existing guests** is on.

## Limits per security group

Settings → **Security group limits** (or `dbcanvas-cli group policy`, or
`PUT /api/security/policies`) sets, for Administrators, Regular Users and Guests:

| Limit | Effect |
| --- | --- |
| Longest lifetime | The longest TTL a stack can be created or extended with. Below `infinity`, no stack is kept forever. |
| Stacks | How many stacks one account may have. |
| Deployed nodes | How many nodes one account may have deployed across its stacks. |
| Node types it may not deploy | Type ids (`k3d`, `kubeadm`, `vnc`, …). A stack holding one fails validation, so it cannot be deployed. |

Empty or 0 means no limit, and every limit is off until an administrator sets one. They apply to
new actions. A stack that already exists is not torn down when a limit tightens.

## API tokens

- **Scope.** A token has a scope: read, write or admin. Admin needs an administrator account.
- **Limit.** A token can also be limited to some stacks: the ones it creates itself
  (`--own-stacks`), and/or the ones carrying given labels (`--limit-label tool=ci`).
  - Such a token can call only those stacks' routes, list (filtered to them), create and compose
    stacks, and read the catalogues. Every other endpoint answers 403.
  - Stacks it creates carry its labels, and it cannot remove them.
- **Use.** Give a program the narrowest token that works: `dbcanvas-cli token create --name ci
  --own-stacks --expires 30`.

## The CLI download

`GET /api/cli/download` serves the `dbcanvas-cli` built into the image, with its SHA-256 in
`X-Checksum-Sha256`. `GET /api/cli/checksums` lists every platform's digest in `sha256sum`
format. `dbcanvas-cli version --verify` checks the binary you are running against that list, and
`dbcanvas-cli version` warns when the CLI and the server are different releases.

## Signed releases

Each GitHub release carries `dbcanvas-cli` for every platform, a `dbcanvas-cli_checksums.txt`, and
a Sigstore bundle (`.sigstore.json`) for each file. They are built and signed by the release
workflow (`.github/workflows/release.yml`) when a version is tagged. The signing is keyless: the
signature records the repository, the workflow and the tag that built the file, so no private key
exists to leak.

Check a download with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/):

```sh
cosign verify-blob dbcanvas-cli_linux_amd64 \
  --bundle dbcanvas-cli_linux_amd64.sigstore.json \
  --certificate-identity-regexp '^https://github.com/jaimesicam/dbcanvas/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`dbcanvas-cli version --verify --release` compares the running binary with the checksum list on its
version's release. It compares digests over HTTPS and does not check the signature; cosign does.
A server's app image builds its own copy of the CLI with the same flags, which matches the release
only when it used the same Go patch release.

## Service passwords

The passwords in `.env` (the database, proxy, PMM and directory accounts every stack is built
with) start at random values on a new installation. An older `.env` may still hold the ones in
`.env.example`, which anybody can read. Administrators then see a notice naming them on every
page. Run `scripts/gen-secrets.sh .env` and `make restart` on the host to replace them. Stacks
deployed before keep their old passwords.

## Secrets at rest

Passwords and stack secrets in the database are encrypted with a key kept on its own volume
(`/keys`). Back up the database and the key separately, and keep both. See
[Configuration](CONFIGURATION.md).

## What is not supported

**A Docker socket proxy.** A proxy that filters the Docker API (for example, one allowing only
some endpoints) does not reduce what an account can do here. DBCanvas needs to create privileged
containers, and it mounts the socket into helper containers for image builds and K3D clusters.
The protection that works is limiting who can reach the installation and who has an account.
