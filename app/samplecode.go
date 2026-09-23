package main

import (
	"fmt"
	"sort"
	"strings"
)

// samplecode.go — Sample Client Code: runnable client programs for a deployment on the canvas.
//
// A DBCanvas stack answers "what does this database do". It could never answer the question
// underneath it — *how do I talk to it from an application* — without the user leaving the app,
// finding a driver's quickstart, and then editing every value in it: the host, the port, the user,
// the password, the TLS arguments. Every one of those is already known here. So this feature does
// the edit instead of the person: pick a deployed endpoint, pick a language and a client library,
// and get a project whose connection details are the ones the stack actually has, on a Linux
// Client that DBCanvas then prepares and runs it on.
//
// ------------------------------------------------------------------- the shape of the registry
//
// A sample is addressed by four axes — **database, language, client, scenario** — and is written
// out as a path: mysql/python/mysql-connector/crud. That is the whole indexing scheme, and it is
// what makes the feature extensible without the UI changing: adding PHP + PDO means adding one
// scClient to scClients below, and the picker, the dependency plan, the licence list and the
// generated project all follow from it.
//
// The three registries here are deliberately separate, because they answer different questions:
//
//   - scClients      — what can be generated, and what code comes out (samplecode_gen.go).
//   - scSysPackages  — how a *runtime or native client* is installed, per Linux distribution
//                      (samplecode_env.go). A sample never names apt or dnf; it names "python3".
//   - scDep          — what a sample pulls in from its own ecosystem at run time, with the licence
//                      and the upstream URL recorded beside it.
//
// The last one is not bookkeeping. DBCanvas is GPL-3.0-only and it does not vendor any of these:
// pip, npm, Go modules, Maven and NuGet fetch them onto the lab node at run time, under their own
// licences, and the generated project files reference them by name and version so the project
// stays reproducible after the install. Nothing in this feature copies third-party source into
// this repository, and nothing about a dependency's licence changes because DBCanvas installed it.
// See docs/SAMPLE_CODE.md, which says so where a user will read it.

// The engine families a sample can target. These are the same four the rest of the app uses
// (engineForType returns the first three; Valkey has never been a "SQL target" so it is not in
// that map), and a target resolves to exactly one of them.
const (
	scMySQL    = "mysql"
	scPostgres = "postgres"
	scMongoDB  = "mongodb"
	scValkey   = "valkey"
)

// scDatabases is the picker's first axis, in the order it is offered.
var scDatabases = []struct{ ID, Label, Blurb string }{
	{scMySQL, "MySQL family", "Percona Server, PXC, MySQL Community and MariaDB — one wire protocol, one set of drivers."},
	{scPostgres, "PostgreSQL", "Percona Distribution for PostgreSQL, Patroni, repmgr and Spock."},
	{scMongoDB, "MongoDB", "Percona Server for MongoDB — standalone, replica set or sharded."},
	{scValkey, "Valkey", "Standalone or cluster."},
}

// scLanguages is the second axis. The label is what the picker shows; the runtime is what decides
// the environment plan (see scRuntime* in samplecode_env.go).
var scLanguages = []struct{ ID, Label, Runtime string }{
	{"python", "Python", scRuntimePython},
	{"node", "Node.js", scRuntimeNode},
	{"go", "Go", scRuntimeGo},
	{"java", "Java", scRuntimeJava},
	{"dotnet", "C# (.NET)", scRuntimeDotnet},
	{"shell", "Shell / native client", scRuntimeShell},
}

// scScenario is the fourth axis: what the program does once it is connected.
type scScenario struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Blurb string `json:"blurb"`
}

var scScenarios = []scScenario{
	{"connect", "Connection Test", "Open a connection, ask the server what it is, close it."},
	{"create", "Create", "Create the customers table (or collection, or keys) and insert one row."},
	{"read", "Read", "Insert a row, then read it back."},
	{"update", "Update", "Insert a row, change it, and read the change back."},
	{"delete", "Delete", "Insert a row, delete it, and confirm it is gone."},
	{"crud", "Full CRUD", "Create, read, update and delete in sequence — the complete program."},
}

// scOps is a scenario expanded into the operations one program performs. Every generator branches
// on this struct rather than on the scenario name, so a new scenario is a new row in scOpsFor and
// not an edit to twenty-three templates.
//
// Seed is the operation that is not the point of the scenario but has to happen anyway: "Read"
// cannot read a row that was never inserted. It is separated from Create so the generated output
// can say "seeding a row to read" rather than pretending the insert is what is being demonstrated.
type scOps struct {
	Schema bool // create the table / collection / key space
	Seed   bool // insert the row this scenario needs but does not itself demonstrate
	Create bool
	Read   bool
	Update bool
	Delete bool
}

// Any reports whether the program does anything beyond connecting.
func (o scOps) Any() bool { return o.Schema || o.Seed || o.Create || o.Read || o.Update || o.Delete }

func scOpsFor(scenario string) scOps {
	switch scenario {
	case "create":
		return scOps{Schema: true, Create: true}
	case "read":
		return scOps{Schema: true, Seed: true, Read: true}
	case "update":
		return scOps{Schema: true, Seed: true, Update: true, Read: true}
	case "delete":
		return scOps{Schema: true, Seed: true, Delete: true}
	case "crud":
		return scOps{Schema: true, Create: true, Read: true, Update: true, Delete: true}
	}
	return scOps{} // connect
}

// scDep is one third-party component a sample resolves from its own ecosystem at run time.
//
// Licence and URL are recorded for every one of them. DBCanvas installs these; it does not ship
// them, and it does not relicense them — which is a distinction worth keeping in the data
// structure and not only in the prose, because it is the question anyone redistributing a
// generated project has to answer for themselves.
type scDep struct {
	Manager string `json:"manager"` // pip | npm | gomod | maven | nuget
	Name    string `json:"name"`    // pip/npm name, Go module path, or group:artifact
	Version string `json:"version"` // pinned where it goes into a generated manifest
	Import  string `json:"import"`  // pip only: the module name, for the "is it installed" check
	License string `json:"license"` // SPDX identifier where the upstream states one
	URL     string `json:"url"`
	Note    string `json:"note,omitempty"` // a licence caveat worth reading before redistributing
}

// scClient is one library (or native tool) for one database in one language: the third axis, and
// the unit a contributor adds.
type scClient struct {
	Database string // scMySQL | scPostgres | scMongoDB | scValkey
	Language string // python | node | go | java | dotnet | shell
	ID       string // "mysql-connector", "hikari", "database-sql", …
	Label    string // "mysql-connector-python"
	Summary  string // one sentence: what this client is, and when to reach for it
	Runtime  string // scRuntime* — what has to be installed for it to run at all
	// SysPackages are logical system packages beyond the runtime's own, by scSysPackages id.
	// A native-client sample names its client here ("mysql-client"); everything else is empty.
	SysPackages []string
	Deps        []scDep
	// Files renders the project. Run is the command that executes it from the project directory.
	Files func(g scGen) []scFile
	Run   func(g scGen) string
	// Scenarios restricts which scenarios this client offers; empty means all of them.
	Scenarios []string
}

// scFile is one file of a generated project.
type scFile struct {
	Name string `json:"name"` // relative to the project directory; may contain "/"
	Lang string `json:"lang"` // for the editor's highlighting: python|javascript|go|java|csharp|xml|shell|text
	Body string `json:"body"`
	// Mode is the file's permission on the node. 0 means 0644; a private key is written 0600.
	Mode int64 `json:"-"`
	// Secret marks a file whose contents must not be written to a log line.
	Secret bool `json:"-"`
}

// scSampleID is the registry's address for one sample, and the form every API and every log line
// uses: database/language/client/scenario.
func scSampleID(database, language, client, scenario string) string {
	return strings.Join([]string{database, language, client, scenario}, "/")
}

// scParseSampleID splits an address back into its four axes. It validates the shape only; whether
// the sample exists is scResolveSample's answer.
func scParseSampleID(id string) (database, language, client, scenario string, ok bool) {
	p := strings.Split(strings.TrimSpace(id), "/")
	if len(p) != 4 {
		return "", "", "", "", false
	}
	for _, s := range p {
		if strings.TrimSpace(s) == "" {
			return "", "", "", "", false
		}
	}
	return p[0], p[1], p[2], p[3], true
}

// scFindClient looks one client up by its three identifying axes.
func scFindClient(database, language, client string) (scClient, bool) {
	for _, c := range scClients {
		if c.Database == database && c.Language == language && c.ID == client {
			return c, true
		}
	}
	return scClient{}, false
}

// scClientOffers reports whether a client implements a scenario.
func (c scClient) offers(scenario string) bool {
	if len(c.Scenarios) == 0 {
		for _, s := range scScenarios {
			if s.ID == scenario {
				return true
			}
		}
		return false
	}
	for _, s := range c.Scenarios {
		if s == scenario {
			return true
		}
	}
	return false
}

// scResolveSample turns an address into the client that renders it, with the error the user should
// read when it does not exist. The three ways to be wrong need three different sentences: a
// database that has no such client at all, a client that does not speak this database (the
// combination the picker is supposed to make unreachable), and a scenario the client has not
// implemented.
func scResolveSample(id string) (scClient, scScenario, error) {
	db, lang, client, scenario, ok := scParseSampleID(id)
	if !ok {
		return scClient{}, scScenario{}, fmt.Errorf("%q is not a sample id — it should read database/language/client/scenario", id)
	}
	c, found := scFindClient(db, lang, client)
	if !found {
		if other := scClientsFor(db); len(other) == 0 {
			return scClient{}, scScenario{}, fmt.Errorf("no samples for database %q", db)
		}
		return scClient{}, scScenario{}, fmt.Errorf("no %s sample for %s using %q", lang, db, client)
	}
	for _, s := range scScenarios {
		if s.ID != scenario {
			continue
		}
		if !c.offers(scenario) {
			return scClient{}, scScenario{}, fmt.Errorf("%s has no %q example", c.Label, scenario)
		}
		return c, s, nil
	}
	return scClient{}, scScenario{}, fmt.Errorf("unknown example %q", scenario)
}

// scClientsFor is every client that speaks one database, in picker order: by language (the order
// of scLanguages), then by the order they are registered, which is the order a person would meet
// them — the ecosystem's usual first choice first.
func scClientsFor(database string) []scClient {
	rank := map[string]int{}
	for i, l := range scLanguages {
		rank[l.ID] = i
	}
	var out []scClient
	for _, c := range scClients {
		if c.Database == database {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Language] < rank[out[j].Language] })
	return out
}

// scLanguageLabel is the display name for a language id.
func scLanguageLabel(id string) string {
	for _, l := range scLanguages {
		if l.ID == id {
			return l.Label
		}
	}
	return id
}

// ---------------------------------------------------------------- the catalogue, as the UI sees it

// scCatalogDTO is the whole registry, flattened for the picker. It carries the dependency and
// licence metadata with each client so the page can show what a choice will install *before* it is
// installed — which is the only moment at which that information is useful.
type scCatalogDTO struct {
	Databases []scCatalogDatabase `json:"databases"`
	Scenarios []scScenario        `json:"scenarios"`
}

type scCatalogDatabase struct {
	ID      string            `json:"id"`
	Label   string            `json:"label"`
	Blurb   string            `json:"blurb"`
	Clients []scCatalogClient `json:"clients"`
}

type scCatalogClient struct {
	ID            string   `json:"id"`       // language/client, the two axes the picker chooses at once
	Language      string   `json:"language"` // python | node | go | java | shell
	LanguageLabel string   `json:"languageLabel"`
	Client        string   `json:"client"`
	Label         string   `json:"label"`
	Summary       string   `json:"summary"`
	Runtime       string   `json:"runtime"`
	Scenarios     []string `json:"scenarios"`
	// Requires is what running this will need on the node, in the words the environment log uses:
	// the runtime and native packages first, then the ecosystem dependencies.
	Requires []string `json:"requires"`
	Deps     []scDep  `json:"deps"`
}

// scCatalog builds the catalogue. Derived from the registry on every call rather than cached: it
// is a few hundred structs, and a stale catalogue is a whole class of bug that need not exist.
func scCatalog() scCatalogDTO {
	out := scCatalogDTO{Scenarios: scScenarios}
	for _, d := range scDatabases {
		cd := scCatalogDatabase{ID: d.ID, Label: d.Label, Blurb: d.Blurb}
		for _, c := range scClientsFor(d.ID) {
			scen := []string{}
			for _, s := range scScenarios {
				if c.offers(s.ID) {
					scen = append(scen, s.ID)
				}
			}
			cd.Clients = append(cd.Clients, scCatalogClient{
				ID:            c.Language + "/" + c.ID,
				Language:      c.Language,
				LanguageLabel: scLanguageLabel(c.Language),
				Client:        c.ID,
				Label:         c.Label,
				Summary:       c.Summary,
				Runtime:       c.Runtime,
				Scenarios:     scen,
				Requires:      scRequirementLabels(c),
				Deps:          c.Deps,
			})
		}
		out.Databases = append(out.Databases, cd)
	}
	return out
}
