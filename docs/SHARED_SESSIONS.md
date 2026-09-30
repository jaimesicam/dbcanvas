# Shared Sessions

A **shared session** lets colleagues watch you work in DBCanvas, live, from their own
browsers — and take the wheel when you hand it to them. You share a link; whoever opens
it gives a name and an email and waits in a lobby until you let them in. From then on
they follow you across every tab, see the terminals and node web UIs you open, and chat
with you. Give one of them control and they can do anything you can in your workspace,
while everyone else watches them.

A link lasts at most **two hours**. Nobody has to install anything or forward a port:
everything, including a VNC desktop, travels through DBCanvas's own address.

![A shared session from the host's side: the stack on the canvas, a VNC desktop shared into a browser window, and the session panel with a guest waiting in the lobby](screenshots/shared-session-host.png)

> [!IMPORTANT]
> A guest you give control to can do everything you can in your workspace — deploy,
> destroy, open a root shell. Admit only people you would hand your keyboard to.

## Turning it on

Shared sessions are **off** until an administrator turns them on in **Settings → Shared
sessions**. The same row sets the longest a link may last (5 to 120 minutes) and how long
the record of an ended session is kept (see [What is kept](#what-is-kept)). Turning the
switch off ends every live session at once.

Guests reach DBCanvas at the address in the link, so it has to be one they can reach:

| Where the guests are | Set in `.env` |
| --- | --- |
| On the same network | `APP_HOST=0.0.0.0` so DBCanvas listens beyond this machine, and `PUBLIC_URL=http://<this machine's address>:8080` so links carry that address |
| Behind an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 you@server`) | Nothing: the link says `localhost:8080`, which is the tunnel on their machine |
| Through a port forward or a proxy | `PUBLIC_URL` set to the forwarded address; websockets must pass through |

Everything a guest uses — the pages, the live channel, terminals, VNC — goes through
that one port. See [Configuration](CONFIGURATION.md) for both variables.

## Starting a session

Open a stack you own and click **Share** in the canvas toolbar. Pick how long the link
lasts, and whether to **Hide secrets** (below), then **Start and get the link**. The
session panel opens beside the workspace with the link and a **Copy link** button — the
link is shown this once.

The dialog also lists the earlier sessions on this stack, each with its transcript.

## Joining

The link opens a join screen. The guest types a name and an email — both required,
neither verified: they are labels for you to recognise people by, not an identity.

![The join screen a guest sees when they open the link](screenshots/shared-session-join.png)

They wait in the **lobby**. You get a notification and an entry at the top of the
session panel with their name, email and the address they came from, and **Admit** or
**Deny**. Nobody sees anything of your workspace before you admit them. A guest who is
denied or removed can only ask again, as a new entry in the lobby for you to decide on.

A guest's tab stays on the `/join/…` address for the whole session; reloading it puts
them back where they were. A colleague who also has an account on this DBCanvas keeps
their own sign-in in their other tabs — only the guest tab is a guest.

## Following

An admitted guest sees DBCanvas as you do and **follows** you: the page you are on, the
stack you have open, the canvas's position and zoom, the node you selected, and your
pointer on the canvas. Untick **Follow** in the panel to look around on your own; tick
it to catch up.

![The same session from a guest's side: following the host, the canvas and the desktop both view only](screenshots/shared-session-guest.png)

The top bar always says who has control. A guest's sidebar leaves out the pages that
belong to your account — Settings, API tokens, Manage Users — and they never see your
notifications.

## Control

One person drives at a time. At first that is you.

- A guest clicks **Request control**; the request appears in the panel with **Grant**
  and **Deny**. You can also give control from the guest's row.
- While a guest drives, everyone — you included — follows them, and your canvas shows
  **Following … — view only**.
- **Take control back** returns it to you at any moment; the guest can **Hand control
  back**. A driver who disconnects keeps control for 30 seconds — a reload, a flaky
  connection — and then loses it.

Whoever does not have control **watches**. A watcher can read everything and change
nothing, and this is enforced by the server, not by greyed-out buttons:

| | Watching | Driving |
| --- | --- | --- |
| Pages, stacks, node panels, results | read | read and change |
| The canvas | view only — edits, deletes, moves and Properties are refused | edit, deploy, destroy |
| Terminals | watch | type |
| Browser windows | look; a VNC desktop live but untouchable | use |
| Close a shared terminal or window | no | yes |

These are never reachable by a guest, driving or not: your account and password, your
API tokens, your notifications, other users, administration and instance settings, and
the session itself (admitting, removing, giving control, ending). An administrator's
guests are not administrators: they see your stacks, not everyone's.

## Terminals

A terminal opened during a session — by the driver, or by you — is **shared**: one shell
on the node, shown to everyone, with what came before it for anyone who arrives late.
Only whoever has control can type into it; when control moves, the keyboard moves with
it. Only the driver can close a shared terminal; anyone else can minimize it to the dock.

## Browser windows

Every node web UI — a VNC desktop, PMM, webmail, a simulator's dashboard, HAProxy stats,
SeaweedFS, OpenEverest — is published on a port of its own, which a guest (or anyone
reaching DBCanvas through one tunnel) cannot reach. **Right-click** any link to one and
choose **Open in browser window**: the page opens in a window inside DBCanvas, served
through DBCanvas's own port. For a guest, a plain click does the same.

![Right-clicking a node's web link: open it in a browser window, a new tab, or copy it](screenshots/browser-window-menu.png)

The window has back, forward, reload, an address you can edit, **Open in a browser tab**
(still through DBCanvas), maximize and close. It works outside a session too.

In a session, a window the driver or you opens is **shared**: it opens on every screen,
follows the driver as they move around in it, and only the driver can close it. A VNC
desktop connects by itself, with its password filled in for anyone allowed to read it,
and in *shared* mode so a second viewer never disconnects the first. A watcher sees it
live and cannot touch it: their connection passes through a filter on the server that
drops keyboard, pointer, clipboard and resize messages.

Pages in a browser window run on DBCanvas's own address. That is what lets them keep
their own logins, and it is fine for the lab services DBCanvas deploys; it is not a
sandbox for arbitrary websites.

## Hide secrets

With **Hide secrets** ticked, every password is masked in what guests are sent — node
secrets, design fields, a VNC desktop's password — and a guest who saves the design while
driving cannot overwrite a real password with the mask they were shown. It keeps
passwords off guests' screens; it does not stop a guest who drives from reading a
configuration file in a terminal.

## Chat

Everyone admitted shares one chat, in the panel. Messages are plain text — whatever a
guest types, `<script>` included, is shown and never run. System events arrive in the
same stream: joins, the lobby, control changes, every change a guest makes while driving
("Jane — Stop a node — POST …"), terminals and windows opened, and warnings ten and two
minutes before the link expires. You can **mute** a guest, who keeps watching, or
**remove** them.

## Ending

A session ends when its time is up, when you click **End session**, or when an
administrator turns shared sessions off. Every guest is disconnected at once and their
browser says so; control returns to you and shared terminals close. The panel then
offers the transcript.

## What is kept

What a session leaves behind is stored in DBCanvas's database:

| Kept | Details |
| --- | --- |
| The session | stack, host, start, expiry, how it ended, whether secrets were hidden |
| Guests | name, email, address, and when they joined, were admitted and left |
| Transcript | every chat message and system event |
| Guest actions | every change a guest made while driving, with its result |

It is kept for **90 days** after the session ends (**Settings → Shared sessions** changes
that; 0 keeps it until the stack is deleted), and deleting the stack deletes it. The link
and each guest's credential are stored only as hashes. Download a transcript as text or
JSON from the panel after a session, or from the Share dialog's list of earlier ones.

Not kept: what was typed or shown in a terminal, what happened inside a VNC desktop or a
web page, and anything live — who was online, the pointer, the windows that were open.

## From the API

Every part of it is an endpoint — starting a session, the lobby, control, the live
channel, transcripts, and `POST /api/browse` for the browser window. See [Shared
sessions in the API reference](API_REFERENCE.md#shared-sessions).
