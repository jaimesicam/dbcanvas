package main

import (
	"net/http"
)

// whatsnew.go — the release notes DBCanvas shows you once, the first time you open
// a build you have not seen.
//
// The notes are Go literals, the same way labs.go and templates_builtin.go carry
// their content. The alternative — parsing the README's "What's new" section at run
// time — was tempting because that prose already exists, but that section is
// <details> blocks, screenshots and relative links written for somebody reading
// GitHub. It is good prose and a bad data source. So these are written separately,
// and whatsnew_test.go asserts that every Title here appears in README.md, which
// catches the one thing that actually goes wrong: the two drifting apart.
//
// Read-state lives on the account (UserSettings.WhatsNewSeen), not in the browser.
// That follows settings.go's own rule — preferences follow the user across browsers
// and machines — and it is the right call here for a specific reason: a localStorage
// flag would re-open this dialog in every new browser and never again after a
// reinstall, which is exactly backwards.

// releaseNote is one entry. Body is plain prose, deliberately: this is read once, in
// a modal, by somebody who wants to know what changed and then get on with it.
type releaseNote struct {
	Version string `json:"version"`
	Date    string `json:"date"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	// Doc is a repo-relative path to the full guide, or "" when the note is the
	// whole story.
	Doc string `json:"doc,omitempty"`
}

// whatsNewNotes is newest first. Keep it that way: the dialog opens the top entry
// expanded, and the tests assume the ordering.
var whatsNewNotes = []releaseNote{
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Several installations on one Docker host",
		Body: "Each installation now has a name, DBCANVAS_INSTANCE, which `make env` sets to dbcanvas-<your " +
			"login>. It names the app image, the volumes and every container, network and K3D cluster the " +
			"installation deploys, so two people on one host no longer overwrite each other's image or collide " +
			"on stack 1. An existing installation keeps the name dbcanvas and everything it already runs. Node " +
			"images are tagged by release; after upgrading, `make adopt-images` tags the ones you already built " +
			"instead of rebuilding them.",
		Doc: "docs/CONFIGURATION.md",
	},
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Live view on the canvas",
		Body: "Live, in the Database Stacks toolbar, opens a panel beside every running node: CPU, memory, disk, " +
			"swap, IOPS, network and disk throughput, and the replication role the database reports right now " +
			"(primary or replica, read-write or read-only) for MySQL, PostgreSQL, MongoDB and Valkey, flagged " +
			"when it contradicts the design. It refreshes every 2, 5 or 10 seconds, and panels can be dragged " +
			"or closed.",
	},
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Ceph block storage for K3D clusters: volumes that grow",
		Body: "A Ceph node (monitor, manager and one OSD in a container) can hold a K3D cluster's volumes. The " +
			"cluster gets Ceph CSI with ceph-rbd as its storage class, so the operator's database volumes are " +
			"RBD images that can grow, from the cluster's Storage panel. The Ceph node's panel shows health, " +
			"capacity and every image with the PVC it backs.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Shared sessions: drawing on the screen, and recordings",
		Body: "Anyone in a shared session can draw lines or put down text on what everyone is looking at, and " +
			"erase their own; the host can erase anything and turn drawing off for a guest or for everyone. The " +
			"host can also record the session, screen, drawings and chat, and the recordings are listed in the " +
			"Share dialog with the chat transcript until they are purged (30 days by default).",
		Doc: "docs/SHARED_SESSIONS.md",
	},
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Vector search at design time: MongoDB and pgvector",
		Body: "PS MongoDB replica sets and sharded clusters can be deployed with vector search (Percona Search " +
			"for MongoDB, mongot, on PSMDB 8.3), as can the PSMDB operator from 1.23.0. PostgreSQL standalone " +
			"nodes, Patroni, repmgr and Spock get a pgvector option, and the Percona, CloudNativePG and Crunchy " +
			"operators create the extension at bootstrap.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Support Sim: a help desk on vector search, on MongoDB or pgvector",
		Body: "A new demo application: a help desk whose search runs on vector search, with a Search Showdown, " +
			"hybrid tuning, a seven-step Vector Lab and an Index Workshop. It runs on MongoDB's mongot or on " +
			"PostgreSQL with pgvector, as the Support Sim and pgvector Support Sim nodes, each with a template.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.15",
		Date:    "2026-10-07",
		Title:   "Also: Relationships, a toolbar that fits, and desktop polish",
		Body: "The Database Stacks toolbar's Relationships button draws the settings made in node properties " +
			"(PMM, Orchestrator, Repository, OpenBao, SeaweedFS, Keycloak, LDAP, Watchtower) as labelled lines, " +
			"and PgBouncer lines are captioned. Zoomed in or in a small window, the toolbar's secondary buttons " +
			"drop their labels so Deploy stays in view. Dragging a window shows a grabbing cursor.",
	},
	{
		Version: "0.0.14",
		Date:    "2026-10-05",
		Title:   "PostgreSQL Query Analytics for PMM: pg_stat_statements or pg_stat_monitor",
		Body: "PMM's Query Analytics needs a statement-statistics extension, and PostgreSQL turns neither on. A " +
			"standalone PostgreSQL node or a Patroni cluster monitored by PMM now picks pg_stat_statements or " +
			"Percona's pg_stat_monitor, and repmgr and Spock clusters can enable pg_stat_statements. The " +
			"extension is preloaded, configured as the PMM documentation sets it up, created in the postgres " +
			"database and passed to pmm-admin as --query-source.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.14",
		Date:    "2026-10-05",
		Title:   "Linux Client: Percona database clients at design time",
		Body: "A Linux Client can be deployed with the Percona Server MySQL client, MySQL Shell, mongosh, " +
			"Percona psql and Percona ClusterSync for MongoDB already installed. The pickers offer only what " +
			"Percona publishes for the node's OS release, and the versions shown are read back off the binaries.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.14",
		Date:    "2026-10-05",
		Title:   "Linux Client: every Kubernetes cluster in one kubeconfig",
		Body: "A running Linux Client copies the admin kubeconfig of every Kubernetes cluster in the stack into " +
			"/root/.kube/config, one context per cluster named after its frame, and switches the current context " +
			"from its panel. Contexts you added yourself are kept when you copy again.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.14",
		Date:    "2026-10-05",
		Title:   "Also: desktop fixes",
		Body: "A terminal opened from a node no longer lands behind the Database Stacks window. The docked " +
			"terminal comes in front when it opens, and the taskbar's Terminals button raises it when a window " +
			"covers it. The minimized Deployment button sits in its window's corner instead of under the taskbar.",
	},
	{
		Version: "0.0.13",
		Date:    "2026-10-01",
		Title:   "DBCanvas is a web desktop",
		Body: "Every page now opens in a window on a desktop: move it, resize it from any edge, snap it to half " +
			"or a quarter of the screen, minimize it to the taskbar, or tile and cascade the lot. The sidebar " +
			"became a Start menu, grouped and searchable, and the desktop has an icon for every page and every " +
			"stack — double-click a stack for a folder of its nodes, with a right-click for a node's console " +
			"and web UIs. Terminals, node web UIs and the file managers are windows too. Appearance → Layout → " +
			"Classic brings back the sidebar and tabs.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.13",
		Date:    "2026-10-01",
		Title:   "Shared sessions: the whole app, and Mirror everything",
		Body: "A session covers the whole workspace and starts from Share on any page. With Mirror everything " +
			"on, everyone sees exactly the driver's screen — menus, dragged windows, dialogs, a VNC desktop " +
			"moving live — and everyone who is not driving always follows. A colleague with an account can " +
			"join as themselves, signed in; anyone else joins as a guest.",
		Doc: "docs/SHARED_SESSIONS.md",
	},
	{
		Version: "0.0.13",
		Date:    "2026-10-01",
		Title:   "Kanban boards",
		Body: "Boards of columns and cards, private or shared. Drag a card to exactly where it belongs with the " +
			"mouse, a touch screen or the keyboard, and drag columns to reorder them. Cards carry a description, " +
			"labels, an assignee, a due date and a colour; columns a colour and a work-in-progress limit.",
		Doc: "docs/KANBAN.md",
	},
	{
		Version: "0.0.13",
		Date:    "2026-10-01",
		Title:   "Profiles: a name, an email and an avatar, and a Profile page",
		Body: "Accounts have a first and last name, an email address (one per account) and an avatar, asked for " +
			"at setup and registration and shown across the app. Name, email, avatar, password and API tokens " +
			"are all on the new Profile page.",
	},
	{
		Version: "0.0.13",
		Date:    "2026-10-01",
		Title:   "Credentials encrypted at rest",
		Body: "Passwords, node and stack secrets, names, emails, chat and Kanban text are encrypted in the " +
			"database, with the key on a volume of its own (app-keys); login sessions are stored hashed. " +
			"Existing data is encrypted on first start. Back up the key with the database, and rotate it with " +
			"make rotate-key.",
		Doc: "docs/CONFIGURATION.md",
	},
	{
		Version: "0.0.13",
		Date:    "2026-10-01",
		Title:   "Also: MariaDB 12.3, node web UIs from the canvas, and fixes",
		Body: "MariaDB 12.3 joins the version picker. Right-click a node with a web UI on the canvas for Open in " +
			"new tab and Open in VNC Browser. Fixed: a watching guest's Leave did nothing while the mirror was " +
			"showing, and dialogs no longer open behind windows.",
	},
	{
		Version: "0.0.12",
		Date:    "2026-09-30",
		Title:   "Shared sessions — work on a stack together, live",
		Body: "Click Share on a stack and send the link. Whoever opens it gives a name and an email and " +
			"waits in a lobby until you admit them; then they follow you across every tab — the page, the " +
			"stack, the canvas, your pointer — and chat with you in the session panel. Give a guest control " +
			"and they can do anything you can in your workspace while everyone else watches; take it back " +
			"with one click. Terminals opened in a session are shared, with only the driver typing. Watching " +
			"is enforced on the server, and a guest never reaches your account, your API tokens or " +
			"administration. A link lasts at most two hours; the transcript is kept for 90 days. Off until " +
			"an administrator turns it on in Settings; set PUBLIC_URL so links carry an address colleagues " +
			"can reach.",
		Doc: "docs/SHARED_SESSIONS.md",
	},
	{
		Version: "0.0.12",
		Date:    "2026-09-30",
		Title:   "Node web UIs in a browser window, through DBCanvas's own port",
		Body: "Right-click any link to a node's web UI — a VNC desktop, PMM, webmail, a simulator dashboard — " +
			"and choose Open in browser window. The page opens inside DBCanvas, served through its own port, " +
			"so a guest or anyone on a single SSH tunnel needs no port forward per UI. In a shared session " +
			"the window opens on everyone's screen and follows the driver; a watcher sees a VNC desktop live " +
			"but cannot type or click in it.",
		Doc: "docs/SHARED_SESSIONS.md",
	},
	{
		Version: "0.0.12",
		Date:    "2026-09-30",
		Title:   "A Refresh button on every tab",
		Body: "Every page that reads from the server has Refresh beside its title. Tabs stay open, so a page " +
			"used to load its lists once — a stack deployed from another tab or a capture finished on the " +
			"server did not show until you reloaded. Refresh re-reads only what came from the server; " +
			"filters, selections and an unsaved canvas stay as they are.",
	},
	{
		Version: "0.0.12",
		Date:    "2026-09-30",
		Title:   "Fixes: dropped terminals, repmgr on Oracle Linux 9 and 10, and dbcanvas-cli jobs",
		Body: "The first benchmark, Query Runner run or Database Explorer connection against a stack no longer " +
			"cuts every open terminal, and neither does tearing the stack down. repmgr installs again on " +
			"Oracle Linux 9 and 10 after a change in PGDG's repository package. dbcanvas benchmark run and " +
			"dbcanvas query run work again.",
	},
	{
		Version: "0.0.11",
		Date:    "2026-09-29",
		Title:   "OpenEverest on Kubernetes",
		Body: "Pick OpenEverest as a K3D frame's operator and DBCanvas installs the database platform " +
			"from its Helm chart, then waits for the Percona operators you tick — MySQL (PXC), " +
			"MongoDB and PostgreSQL — to be installed into Everest's database namespace by OLM. Their " +
			"versions come with the chart release, and the node's panel shows what landed. The frame " +
			"creates no database: that is what Everest's UI is for, and the panel opens it on a " +
			"localhost port, the way a PMM node's is; sign in as admin with EVEREST_PASSWORD. " +
			"Backups and PMM are configured inside Everest. Chart 1.15.0 is left out: its server " +
			"serves a blank page instead of the UI. Renaming a K3D frame no longer leaves its old " +
			"cluster running, holding host ports, after the stack is gone.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.11",
		Date:    "2026-09-29",
		Title:   "Repository node — a local mirror of the Percona repositories and the operator images",
		Body: "A Repository node mirrors only the slice of repo.percona.com a design needs — the OS " +
			"releases, architectures, repositories, versions and packages you list, with the packages " +
			"they depend on — plus Percona operator releases, Helm charts and container images in a " +
			"registry of its own. Point a node or a K3D frame at it and it installs from there " +
			"instead of the internet: RPMs keep Percona's signatures, apt repositories are signed " +
			"with the node's own key, and a k3s cluster pulls its images through it, falling back " +
			"upstream for anything it does not hold. Add packages to a running Repository without " +
			"redeploying it.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.11",
		Date:    "2026-09-24",
		Title:   "EOL releases: CentOS 7 for the Linux Client, and a PMM2 node",
		Body: "A new EOL switch in .env, off by default, offers releases past their end of life the way " +
			"EXPERIMENTAL offers unfinished ones. It only decides what the pickers show, and never " +
			"breaks a stack already built with it on. With it on, the Linux Client runs CentOS 7 — " +
			"repositories pointed at vault.centos.org, percona-release installed — and Sample Client " +
			"Code runs there in Python, Node.js, Go and Java with the mysql, psql and mongosh " +
			"clients; C# and the Valkey client cannot run on CentOS 7 and are greyed out with the " +
			"reason. PMM2 gets a node of its own under Monitoring, releases 2.25.0 to 2.44.1.",
		Doc: "docs/CONFIGURATION.md",
	},
	{
		Version: "0.0.10",
		Date:    "2026-09-23",
		Title:   "PgBouncer — connection pooling for the PostgreSQL family",
		Body: "A PgBouncer node pools for one backend drawn on the canvas: a standalone PostgreSQL " +
			"node, or a Patroni, repmgr or Spock cluster. It sits beside HAProxy rather than " +
			"replacing it — HAProxy balances TCP and leaves a hundred clients as a hundred backend " +
			"processes; PgBouncer terminates the protocol so they share a few dozen server " +
			"connections. A pooler has no health checks, so the node follows the primary itself: a " +
			"timer asks the cluster who takes writes and reloads the pool, and a failover never " +
			"drops a client. Pick the pool mode, the read/write routing and, once the node has a " +
			"certificate, certificate authentication — at the pool and, with a new ordered pg_hba " +
			"method list, at the PostgreSQL servers behind it. The Car Rental Sim, the Stock Market " +
			"Sim and the Ledger Sim can all drive a database through it, and each is warned about " +
			"the one thing transaction pooling breaks: prepared statements cached per connection. " +
			"`dbcanvas stack compose` builds it as `pgbouncer`.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.10",
		Date:    "2026-09-22",
		Title:   "Data-at-rest encryption for PXC clusters and Percona Server replication",
		Body: "The standalone Percona Server node could keep its keyring in OpenBao and a cluster " +
			"could not, which is backwards — a three-node cluster is where a real keyring deployment " +
			"is interesting. Tick Encrypt with OpenBao on a PXC or Percona Server replication frame " +
			"and every member is wired to it: component_keyring_vault on 8.4, the keyring_vault " +
			"plugin on 8.0, and a KV mount of its own for each server, because Percona is explicit " +
			"that a secret_mount_point must serve exactly one instance. A PXC cluster encrypts its " +
			"cluster traffic too — with a keyring configured, PXC's SST script refuses an unencrypted " +
			"channel, so a keyed cluster without it bootstraps one member and never adds a second — " +
			"using one certificate from the Intranet CA, identical on every member as PXC requires. " +
			"The keyring is staged before " +
			"each member's first start rather than added afterwards — the difference between " +
			"configuring a cluster and restarting members out of it one at a time — and the deploy " +
			"proves it: every member is checked for a loaded keyring, and the writable one creates " +
			"and drops a real encrypted table, which is what stores a master key in OpenBao. Two " +
			"bugs fell out of doing it that way: the old path appended early-plugin-load to " +
			"/etc/my.cnf, a file nothing reads on Ubuntu, and OpenBao published its own DNS record " +
			"only at the end of its provisioning, so a database waiting for it deadlocked against it.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.10",
		Date:    "2026-09-23",
		Title:   "Sample Client Code in C#",
		Body: "C# joins Python, Node.js, Go, Java and the shell: MySqlConnector for the MySQL family, " +
			"Npgsql for PostgreSQL, the MongoDB C# Driver and StackExchange.Redis for Valkey, with " +
			"every scenario and every TLS posture the other languages have, mutual TLS included. " +
			"The .NET SDK comes from each Linux release's own archive where it has one — .NET 10 " +
			"on Oracle Linux 8, 9 and 10 and Ubuntu 24.04, .NET 8 on Ubuntu 22.04 — and on Debian, " +
			"which packages none, from Microsoft's SDK archive with its checksum pinned and no " +
			"repository added. The generated project targets .NET 8 and rolls forward, so the same " +
			"project runs on either. Run on all seven releases before it shipped: every client, " +
			"every scenario.",
		Doc: "docs/SAMPLE_CODE.md",
	},
	{
		Version: "0.0.10",
		Date:    "2026-09-23",
		Title:   "The Core Dump Analyzer shows every thread, opens values, and names the line that faulted",
		Body: "All threads is `thread apply all bt` with the two things that command cannot do: " +
			"identical stacks fold together — twenty idle workers in the same wait become one row " +
			"saying 20× — and each carries its real depth. `full` lists every frame's arguments " +
			"and locals under it. Values open now: a struct into its fields, a pointer into what " +
			"it points at, each with the expression to paste into the console. The source pane " +
			"works on real builds, whose recorded paths run through directories that only existed " +
			"on the build machine. And the verdict reads the address the process touched from the " +
			"core itself — null, a null plus a field offset, or never a pointer — matches it to the " +
			"faulting frame's variables and shows that line of source with their values.",
		Doc: "docs/CORE_DUMP_ANALYZER.md",
	},
	{
		Version: "0.0.10",
		Date:    "2026-09-23",
		Title:   "Check for updates, when you ask and not otherwise",
		Body: "The dashboard has a Check for updates button, and it is the only thing in DBCanvas " +
			"that contacts GitHub — nothing checks at startup or on a timer, so an installation " +
			"nobody clicks it on never makes a request off the machine. When there is a newer " +
			"version it opens the release notes for every version between this one and the " +
			"latest, so skipping a release does not hide what was in it. `dbcanvas updates` is the " +
			"same check from the command line.",
		Doc: "docs/API_REFERENCE.md",
	},
	{
		Version: "0.0.10",
		Date:    "2026-09-23",
		Title:   "Exported templates no longer carry the MongoDB Cluster Admin passwords",
		Body: "Exporting a template scrubs every secret from the design, and two were missing from " +
			"the list: the admin and read-only passwords of an MClusterAdmin node, so a template " +
			"exported from a stack with one carried a live MongoDB password with it. Both are " +
			"scrubbed now. A template exported before this release may still hold them — if you " +
			"shared one, change those passwords on the stack it came from.",
	},
	{
		Version: "0.0.9",
		Date:    "2026-09-21",
		Title:   "Upgrading to 0.0.9 — rebuild the Stock Market Sim and the Oracle Linux 10 image",
		Body: "Two of this release's fixes live inside images rather than in DBCanvas itself, so " +
			"`git pull` alone does not deliver them. Run `make stocksim-image` for the Stock Market " +
			"Sim: following a cluster's primary and the new App health panel are compiled into that " +
			"app, and a node deployed from the old image keeps the old behaviour. Run `make images` " +
			"— or rebuild the Oracle Linux 10 base alone — for the EL10 fix: the distro's " +
			"perl-DBD-MySQL was dragging the distro's MySQL libraries into the image, which is what " +
			"stopped Percona Server and PXC installing there, and the repair is a line in the " +
			"Dockerfile. Everything else takes effect on restart. Nodes already deployed are not " +
			"changed by any of this: redeploy the ones you want the new behaviour on.",
		Doc: "docs/GETTING_STARTED.md",
	},
	{
		Version: "0.0.9",
		Date:    "2026-09-21",
		Title:   "Data-at-rest encryption for the PXC and MongoDB operators",
		Body: "The PostgreSQL operator could encrypt at rest and the other two could not, which was " +
			"an odd place for the line to fall. Both now offer it from the same OpenBao node on the " +
			"canvas, with their own KV v2 mount and a token scoped to it — never OpenBao's root " +
			"token, which is what both operators' own examples use. PXC gets a Secret holding " +
			"keyring_vault.conf; MongoDB gets the two halves the operator keys off separately, and " +
			"a sharded cluster's replica set and config servers get separate keys, because they are " +
			"separate WiredTiger deployments and one shared key path has whichever starts second " +
			"overwrite the first's. The keyring file's format follows the SERVER version, not the " +
			"operator: PXC 8.4 reads it as the component's JSON and 8.0 as the plugin's key=value, " +
			"and the wrong one crash-loops every pod before the cluster forms.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.9",
		Date:    "2026-09-21",
		Title:   "A repmgr cluster you can actually switch over, and a tab that tells you how",
		Body: "`repmgr standby switchover` is the one repmgr operation that is not a database " +
			"operation — it stops PostgreSQL on the other machine — so it shells out to SSH, and a " +
			"cluster without it stopped at \"unable to connect via SSH to host …, user\". Every " +
			"repmgr cluster now gets its own keypair at deploy, with the public half in every " +
			"member's authorized_keys including its own, so switchover works in whichever direction " +
			"you choose; repmgr.conf gets the matching ssh_options and the service commands that " +
			"make it stop PostgreSQL with systemctl rather than the pg_ctl systemd would undo. " +
			"Underneath was a second fault: the base images trim the systemd unit whose only job is " +
			"removing /run/nologin, so PAM refused every non-root login for the life of the " +
			"container. Each member also gets a repmgr tab — cluster show, node check, the " +
			"switchover dry run before the real one, promote/follow/rejoin and repmgrd control — " +
			"with the config path, the binary and the peer list already filled in.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.9",
		Date:    "2026-09-21",
		Title:   "A repmgr cluster picks its backup engine",
		Body: "repmgr could only back up with barman-cloud, which was an odd place for the choice to " +
			"be made: every other PostgreSQL kind here uses pgBackRest, so the one cluster type " +
			"whose subject is controlled failover was also the one whose backup tool differed from " +
			"everything you would compare it against. The frame now offers either, and the trade is " +
			"one line — pgBackRest is what the standalone and Patroni clusters use, but its S3 " +
			"client only speaks HTTPS, so the SeaweedFS node needs S3 TLS on; barman-cloud works " +
			"against a plain-HTTP store. Both engines at once is refused, because PostgreSQL has a " +
			"single archive_command and one would silently win. Either survives a switchover: the " +
			"standbys inherit the archive command when they clone, so an incremental taken from the " +
			"new primary references the full backup the old one took.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.9",
		Date:    "2026-09-21",
		Title:   "Percona Server installs on Oracle Linux 10",
		Body: "It did not, and for two unrelated reasons with one symptom. The base image was " +
			"carrying the distro's MySQL: percona-toolkit needs perl(DBD::mysql), the EL10 Percona " +
			"Toolkit repo does not build it, so dnf took the distro's build — which links " +
			"libmysqlclient and drags mysql8.4-libs in behind it, deadlocking every Percona Server " +
			"and PXC 8.4 or 9.7 install. Separately, Percona's own 8.0 el10 build still carries " +
			"unversioned Obsoletes on mariadb-server and friends, which on EL10 resolve to the " +
			"renamed mariadb11.8 packages and collide over /var/lib/mysql. The first is fixed in " +
			"the image and needs `make images`; the second at install time, for EL10 only. PXC does " +
			"not need the second — there is no PXC 8.0 el10 build to hit it.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.9",
		Date:    "2026-09-21",
		Title:   "The Stock Market Sim follows a cluster's primary, and says when it is stuck",
		Body: "Pointed straight at a repmgr or Patroni cluster, the sim was given the member that " +
			"happened to be primary at deploy. After a switchover it reconnected to that same host " +
			"— now a read-only standby — and every write failed while every read kept working, so " +
			"the dashboard drew a live-looking market that had not written a row in an hour. Its " +
			"DSN now names every member and asks for the one accepting writes, and the store drops " +
			"its pooled connections if it ever finds itself on a standby. The dashboard gained an " +
			"App health panel for the same reason the failure went unnoticed: writes/s and reads/s, " +
			"an error count with when it last happened and a button to clear it, and a stalled " +
			"flag that turns red within ten seconds whatever the cause. The simulation's writes are " +
			"counted apart from backfill's bulk history, which otherwise buries them.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.8",
		Date:    "2026-09-17",
		Title:   "Every member of an operator's replica set, not just the one in front",
		Body: "A MongoDB cluster an operator deployed without a router was invisible to all four " +
			"database tools, however it was exposed. Its members are published one Service per pod " +
			"— k3d-03-rs0-0, -1, -2 — and the name-matching that recognises an operator's Services " +
			"had no pattern for that, so a replica set with three LoadBalancer addresses on the " +
			"stack's own subnet contributed nothing, while a sharded cluster beside it was visible " +
			"through its mongos. A Service is now recognised by the pod it selects rather than by " +
			"the shape of its name, which holds for any operator and any replica set name. Every " +
			"member is offered, not only the one that can take writes: a secondary answers reads on " +
			"its own address exactly as the primary does, and a write sent to the wrong one is " +
			"refused by the server in as many words. Which member is primary is not recorded, " +
			"because it changes on failover and a cached answer would be a confident wrong one.",
		Doc: "docs/DATABASE_EXPLORER.md",
	},
	{
		Version: "0.0.8",
		Date:    "2026-09-17",
		Title:   "The Data Generator and the Benchmark reach operator MongoDB",
		Body: "Both tools could see an operator's MongoDB and neither could use it. The Data " +
			"Generator required the exec route and so offered PostgreSQL only — a limit that came " +
			"from how its SQL engines run a client inside the pod, and never applied to its MongoDB " +
			"backend, which dials with the driver over the stack network like every other load tool " +
			"here. The Benchmark built its MongoDB connection from a container id, which a Service " +
			"does not have, so every run died in preparation with \"could not resolve node address\". " +
			"Both now take the address off the endpoint. The Query Runner, which is SQL-only and " +
			"says so, no longer lists MongoDB endpoints it would refuse at Run.",
		Doc: "docs/DATA_GENERATOR.md",
	},
	{
		Version: "0.0.8",
		Date:    "2026-09-17",
		Title:   "Sample Client Code runs on every supported Linux release",
		Body: "Twenty-three samples across seven base images is a hundred and sixty-one programs, " +
			"and a third of them did not compile or connect. Almost all of it came from one thing: " +
			"the environment plan knew the distribution but not the release, so Oracle Linux 8 and " +
			"10 were handed the same package list, and every check asked whether a binary existed " +
			"rather than whether it was new enough. EL8's module streams hid Percona's own clients " +
			"behind modular filtering and pinned Python at 3.6 and Node at 10; Ubuntu 22.04's " +
			"default JDK is 11 while the generated pom compiles at 17; the drivers' own go.mod files " +
			"require Go 1.24, which is newer than Debian 12 or Ubuntu 22.04 ship. The plan now reads " +
			"the release, and every check asks the question the build will ask — whether javac can " +
			"target 17, whether node can parse the syntax the drivers use. Where a distribution " +
			"cannot answer at all, the runtime comes from the project that publishes it, pinned to a " +
			"version and a SHA-256 per architecture, with no third-party repository added to the node.",
		Doc: "docs/SAMPLE_CODE.md",
	},
	{
		Version: "0.0.7",
		Date:    "2026-09-15",
		Title:   "Database Explorer — a database client for the stack you already deployed",
		Body: "A browser-based client for the databases on your canvas, and the point of it is that " +
			"you never tell it anything: DBCanvas provisioned them, so it already holds the address, " +
			"the port, the account and the password, and it reaches them over the stack's own network " +
			"with no port published to your host. MySQL, PostgreSQL, MongoDB, Valkey and ClickHouse, " +
			"with an adapter each rather than one shape forced on all five — MongoDB has a find and an " +
			"aggregation editor and keeps its documents nested, Valkey has a key browser that pages " +
			"with SCAN and a viewer per data type, and the SQL engines get an editor, an Explain and a " +
			"result grid that shows NULL as something other than an empty string, keeps a wide integer's " +
			"digits, and does not fall over on fifty thousand rows. Any result with a number in it can " +
			"become a chart, and the chart says so when the numbers on it are the browser's rather than " +
			"the query's.",
		Doc: "docs/DATABASE_EXPLORER.md",
	},
	{
		Version: "0.0.7",
		Date:    "2026-09-15",
		Title:   "PMM Server's own PostgreSQL and Query Analytics, read-only",
		Body: "A PMM Server carries two databases that are worth reading and normally invisible: " +
			"pmm-managed's inventory, and the ClickHouse behind Query Analytics. Both listen on " +
			"127.0.0.1 inside the container, so DBCanvas reaches them by running their own clients in " +
			"there and asking for machine-readable output rather than the tables they print by default. " +
			"They are read-only, and read-only at the database rather than by a keyword filter: " +
			"PostgreSQL runs inside a READ ONLY transaction and refuses a write with SQLSTATE 25006, " +
			"ClickHouse runs with readonly=2 and refuses one with error 164 — and refuses to lift that " +
			"setting on itself. An administrator can unlock writes for the installation when a scenario " +
			"needs them, and even then a query tab has to be armed for it, so a tab left open from " +
			"before cannot write into PMM by pressing Run.",
		Doc: "docs/DATABASE_EXPLORER.md",
	},
	{
		Version: "0.0.7",
		Date:    "2026-09-15",
		Title:   "The four database tools can target Kubernetes operator clusters",
		Body: "The databases a Percona, CloudNativePG or Crunchy operator deployed inside a K3D frame " +
			"are now targets for the Database Explorer, the Data Generator, the Query Runner and the " +
			"Benchmark. Nothing is read off the canvas: the Services, the pods and the credentials come " +
			"from the cluster and the operator's own Secrets. A LoadBalancer Service is dialled directly " +
			"— MetalLB's address is on the stack's own subnet — and a ClusterIP one, which is the " +
			"operator default and has no address outside the cluster at all, is read by running the " +
			"database's client inside its pod. The two load tools need a real socket, so for them there " +
			"is Expose for tools: a Service added beside the operator's own, never over it, and removed " +
			"again as easily. This is new and wants more use before it is trusted — see the guide.",
		Doc: "docs/DATABASE_EXPLORER.md",
	},
	{
		Version: "0.0.5",
		Date:    "2026-09-13",
		Title:   "Kubernetes States — a cluster as a board that keeps what died",
		Body: "A new page beside Operator Summary, and the opposite tense: that one reads a capture " +
			"after the fact, this one watches a cluster now. One column per kind, one card per object, " +
			"and three things a table cannot do. A card is red because the object says it is broken, " +
			"with the row that caused the colour red inside it. A value that changed since the last " +
			"sample is lit and says what it changed from — \"Ready 2/3 (was 3/3)\" is a failover, " +
			"visible without reading anything. And an object that disappears is not removed: it stays " +
			"where it was, greyed and dashed, until you dismiss it, because the pod that vanished while " +
			"you were looking at another card is the one you needed to see. Beside the board is a rail " +
			"of panes — an object's State, a container's Logs, its YAML — and any of them can be pinned, " +
			"so the custom resource's state and the crashing container's log stay on screen while you " +
			"click through everything else.",
		Doc: "docs/KUBERNETES_STATES.md",
	},
	{
		Version: "0.0.5",
		Date:    "2026-09-13",
		Title:   "The same board reads a pt-k8s-debug-collector cluster-dump",
		Body: "Point it at a capture kept by Diagnostics, or upload a cluster-dump from your machine — " +
			"a customer's cluster this installation has never seen reads exactly the same, because the " +
			"collector's YAML and kubectl's JSON are the same objects in two encodings. Every object in " +
			"the archive becomes a card, including the kinds a live sample leaves alone, so a capture of " +
			"an operator DBCanvas has never heard of still comes out complete. A pod offers what the " +
			"collector kept for it, which is more than kubectl logs would ever have given you: the logs, " +
			"pt-mysql-summary's output, and PXC's innobackup backup logs or pgBackRest's own. Capture " +
			"files lists every file in the archive so nothing is hidden, the board says plainly that a " +
			"capture is one instant and reports what the collector itself failed to collect, and ticking " +
			"compare before loading a second capture turns the whole thing into a diff of the first. " +
			"Uploads are held in memory for an hour, for the account that uploaded them, and never " +
			"written to disk.",
		Doc: "docs/KUBERNETES_STATES.md",
	},
	{
		Version: "0.0.5",
		Date:    "2026-09-13",
		Title:   "make install no longer re-probes every repository it just deleted",
		Body: "A first run took people upwards of three hours, and most of it was spent rebuilding a " +
			"catalog it had thrown away seconds earlier: `make images` wrote versions.yaml, so every " +
			"image rebuild discarded everything `make versions` had probed, and `make install` had to " +
			"run the probe again to get it back. The two files are now separate — images.yaml is what " +
			"was built, versions.yaml is what is installable on it — and neither overwrites the other. " +
			"`make install` no longer runs the probe at all: the catalog committed to the repo survives, " +
			"so the version pickers are populated the moment DBCanvas comes up. Run `make versions` " +
			"deliberately, when you want minors released since the last probe or have built an OS image " +
			"that was not there before. An installation that predates the split keeps working: its image " +
			"entries are still read out of versions.yaml until the next `make images`.",
		Doc: "docs/GETTING_STARTED.md",
	},
	{
		Version: "0.0.4",
		Date:    "2026-09-11",
		Title:   "The Stock Market Sim can split its reads to HAProxy's read port",
		Body: "Every simulator resolved an HAProxy target to one endpoint — the write port — so a " +
			"Patroni or repmgr cluster behind HAProxy took its whole query load on the primary while the " +
			"replicas sat idle. Tick \"Send reads to HAProxy's read port\" on a Stock Market Sim linked to " +
			"an HAProxy node and the queries that only display — the dashboard, the lists, the report — go " +
			"to :5001, which round-robins the replicas, while writes keep :5000. A read whose answer " +
			"decides a write stays on the primary whatever the setting, so replication lag cannot turn " +
			"into a write that fails for a reason nothing on screen explains. For every HAProxy-fronted " +
			"cluster, PostgreSQL and MySQL alike.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.4",
		Date:    "2026-09-11",
		Title:   "A file manager for SeaweedFS buckets",
		Body: "The SeaweedFS node's Buckets tab could show you that a backup landed, and nothing else. " +
			"\"Files…\" on that tab — or \"Bucket file manager\" on the node's right-click menu — now opens a " +
			"two-pane file manager over the buckets themselves: download an object to your machine, upload " +
			"files into a folder by drag-and-drop (under the same size ceiling as a node file drop), delete " +
			"what you no longer want (it asks first, and takes a folder's contents with it only when you say " +
			"so — a folder here is a whole backup), and with the second pane open, copy objects from one " +
			"bucket into another, on this node or another SeaweedFS node in the stack. A copy is streamed container to container, so nothing lands on the " +
			"DBCanvas host on the way, and what you write is an ordinary S3 object — a database node's " +
			"`aws s3 ls` sees it with the key, size and ETag you would expect.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-10",
		Title:   "Edit cr.yaml as a form, generated from the operator's own CRD",
		Body: "A Kubernetes server node's panel has a new cr.yaml tab: the live custom resource as a " +
			"form built from the CustomResourceDefinition that cluster is running — so it offers what " +
			"this operator version accepts rather than a fixed list that goes stale a release later. " +
			"Search every section at once (pitr, size, resources), edit with real controls, and send " +
			"nothing until you say so: the footer counts the pending changes, Review shows the exact " +
			"patch, Check validates it against the API server without applying anything. Fields no " +
			"form can usefully draw — affinity, tolerations, sidecars — are still there as JSON, so " +
			"the whole resource is reachable. For the four Percona operators.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-10",
		Title:   "Point-in-time recovery, two object stores, and kubectl on a Linux Client",
		Body: "A PXC-operator cluster can now run the operator's binlog collector, with a bucket of " +
			"its own for the binary logs — two clusters sharing one produce a stream neither can " +
			"replay. On the replica end of a replication link it starts switched off and DBCanvas " +
			"turns it on once replication is actually running, because the seed restore replaces the " +
			"GTID history the collector would be uploading. A replication pair may also use one " +
			"SeaweedFS node each now, which is the shape two sites really have. And a Linux Client " +
			"can be deployed with kubectl and Helm already on it — kubectl matched to the k3s release " +
			"of a Kubernetes cluster on the same canvas, because it is only supported one minor " +
			"version either side of the API server.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-09",
		Title:   "Replicate one Kubernetes cluster into another",
		Body: "Draw a link between two Kubernetes frames that both run the PXC operator, pick a " +
			"direction, and Deploy: the second cluster becomes a replica of the first. DBCanvas runs " +
			"Percona's own two procedures in the order they have to happen in — the source declares " +
			"the channel and exposes its database pods so a replica can reach them, both clusters " +
			"build at the same time, a backup of the source is taken and restored onto the replica, " +
			"and only then is the replica's channel attached. Attaching it any earlier points the " +
			"replica at binary logs the source has already purged. The server node's new Replication " +
			"tab says which end a cluster is and whether the channel is running; Deploy reconciles the " +
			"channel and never re-seeds, because a seed replaces the replica's data — that is its own " +
			"button. Start from the new two-cluster template.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-08",
		Title:   "MClusterAdmin — a MongoDB administration panel",
		Body: "A node that runs MClusterAdmin, a third-party web panel for MongoDB: topology and " +
			"replica-set status, sharding and the balancer, current operations, slow queries with " +
			"explain, indexes and profiling, users and roles, and oplog stats. Its UI is published to " +
			"a host port like PMM's, so it opens straight from your browser. Every MongoDB node and " +
			"cluster now has an Add MClusterAdmin credentials tick, which creates the two accounts the " +
			"panel expects — one for everything it does, one read-only for the dashboards — with " +
			"upstream's own least-privilege roles, neither of which can drop a database.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-08",
		Title:   "Big Hole — MongoDB FTDC in the browser",
		Body: "A node that runs Big Hole, a third-party viewer for MongoDB's diagnostic.data: drag a " +
			"folder — or a whole support tarball, however it is nested — onto the page and it decodes " +
			"and charts it, every metric it can find, with the replica set laid out so an election is " +
			"somewhere to land rather than something to hunt for. There is no backend at all: nothing " +
			"you open in it leaves your browser, and it talks to no database, no API, not even to " +
			"DBCanvas. Open it on localhost — browsers only grant a page the on-disk storage it needs " +
			"there, and the node's panel hands you the ssh -L line when you are browsing from elsewhere.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-08",
		Title:   "Fixes across the app",
		Body: "A MongoDB node's log and its diagnostic.data are now one download, arriving together " +
			"under a directory named after the node instead of as two files that collide. The Core " +
			"Dump Analyzer shows the whole source file rather than a window around the crashing line, " +
			"and hands back the gdb command line that reproduces the session in your own terminal. A " +
			"long menu item wraps instead of being cut off mid-word. Three sidebar icons that were " +
			"drawing the wrong thing were redrawn. The Intranet image is built by make images, where " +
			"it belongs. And the API page now teaches the CLI — download it, put it on your PATH, sign " +
			"in, and read every command — rather than sending you elsewhere to find out how.",
	},
}

// notesNewerThan returns the notes a reader has not seen. seen == "" means they have
// seen nothing, which is every note.
func notesNewerThan(seen string) []releaseNote { return notesNewerThanIn(whatsNewNotes, seen) }

func notesNewerThanIn(notes []releaseNote, seen string) []releaseNote {
	out := []releaseNote{}
	for _, n := range notes {
		if seen == "" || compareVersions(n.Version, seen) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// hasUnseenNotes decides whether the dialog opens by itself.
//
// The decision is made here rather than in the browser on purpose: it needs version
// comparison, the client would have to reimplement compareVersions to do it, and a
// client that gets that subtly wrong shows a dialog nobody asked for on every page
// load.
//
// Both conditions are required, and the second is the one that is easy to forget:
// the build has to have moved on from what this account acknowledged, AND there has
// to be a note it has not read. Shipping a release with no note written for it shows
// nobody anything, which is the right outcome — an empty dialog is worse than none.
func hasUnseenNotes(seen, current string) bool {
	return hasUnseenIn(whatsNewNotes, seen, current)
}

func hasUnseenIn(notes []releaseNote, seen, current string) bool {
	// Nothing to show once the account has acknowledged this build. A `seen` ahead
	// of `current` means the instance was rolled back, which is also nothing to show.
	if seen != "" && compareVersions(seen, current) >= 0 {
		return false
	}
	return len(notesNewerThanIn(notes, seen)) > 0
}

func (a *App) handleWhatsNew(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	seen := a.userSettingsFor(u.ID).WhatsNewSeen
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   appVersion,
		"seen":      seen,
		"hasUnseen": hasUnseenNotes(seen, appVersion),
		"notes":     whatsNewNotes,
		// What the dialog opens with when it opens by itself. The link in the
		// dashboard header shows everything instead.
		"unseen": notesNewerThan(seen),
	})
}

// handleWhatsNewSeen records that the account has read the notes for this build.
//
// It exists as its own endpoint rather than leaving the client to PUT
// /api/me/settings because the dialog would otherwise have to hold, and echo back,
// the whole settings object to change one field — and a stale copy of that object
// would quietly revert somebody's theme.
func (a *App) handleWhatsNewSeen(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	s := a.userSettingsFor(u.ID)
	s.WhatsNewSeen = appVersion
	if err := a.saveUserSettings(u.ID, s); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seen": s.WhatsNewSeen})
}

// stampWhatsNewSeen marks a brand-new account as having already seen this build's
// notes. A first-time user has no "new" to be told about — everything is new — and
// opening a changelog over somebody's first look at the app is the wrong welcome.
// Best-effort: failing to stamp only means they see the dialog once.
func (a *App) stampWhatsNewSeen(userID int64) {
	s := a.userSettingsFor(userID)
	s.WhatsNewSeen = appVersion
	a.saveUserSettings(userID, s)
}
