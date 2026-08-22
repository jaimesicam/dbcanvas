package main

// logsummary_model.go — several nodes' logs as one timeline.
//
// A single log read on its own answers "what happened here". Three logs read together
// answer the question people actually have, which is "what state was the cluster in at
// 01:49:35, and which node is telling the truth about it".
//
// That question needs three things this file builds:
//
//	events  every classified record from every source, merged and renumbered in time order
//	phases  a continuous state track per source, so any instant can be looked up rather
//	        than reconstructed by eye from the transitions around it
//	buckets per-source severity counts over a time grid, which is what the swimlane draws
//
// Clocks are the quiet hazard. MySQL writes RFC3339 with an explicit zone (Z under the
// default log_timestamps=UTC, an offset under SYSTEM), so records from nodes in different
// timezones land on the correct absolute instant without anyone configuring anything.
// What can still go wrong is host clock skew, which no amount of parsing fixes — so each
// source carries an adjustable offset, and lsOverlap reports when two sources do not
// overlap at all, which is the shape of "you uploaded logs from different days".

import (
	"sort"
	"strings"
)

// lsSource is one log file: one node's view of the cluster.
type lsSource struct {
	Idx     int     `json:"idx"`
	Name    string  `json:"name"`    // file name, or the node label it was read from
	Node    string  `json:"node"`    // the node's own name, discovered in the log
	Engine  string  `json:"engine"`  // mysql | postgres | mongodb | valkey
	Flavour string  `json:"flavour"` // galera | mysql
	Path    string  `json:"path,omitempty"`
	Origin  string  `json:"origin"`           // upload | node
	NodeID  string  `json:"nodeId,omitempty"` // canvas node id, when collected from one
	Bytes   int     `json:"bytes"`
	Lines   int     `json:"lines"`
	Records int     `json:"records"` // folded records, before noise is dropped
	Events  int     `json:"events"`  // classified events kept
	FirstTS float64 `json:"firstTs"`
	LastTS  float64 `json:"lastTs"`
	// Offset is added to every timestamp from this source, in seconds. It exists for
	// host clock skew, which is the one misalignment parsing cannot fix.
	Offset float64        `json:"offset"`
	Counts map[string]int `json:"counts"` // severity → number of events
	// ReadAt is when this source was tailed from its node, in epoch seconds (0 = uploaded,
	// so unknown). It is the source's coverage end whenever it is later than the last
	// record — see lsInput.ReadAt.
	ReadAt float64 `json:"readAt,omitempty"`
	// Untimed is the number of records whose header carried no timestamp. They inherit
	// the previous record's, which is close enough to place them but not to trust to the
	// millisecond, and saying how many there were is more honest than silently placing them.
	Untimed int `json:"untimed"`
	// Mongo is the slow-query arithmetic for a mongod source: six million lines nobody
	// reads, added up. Nil for every other engine. See logsummary_mongo_slow.go.
	Mongo *lsMongoStats `json:"mongo,omitempty"`
	// MongoCfg is how this member was configured, read from its own startup lines. See
	// logsummary_mongo_config.go — it is the only part of the bundle that can say what a
	// setting WAS rather than what its effects looked like.
	MongoCfg *lsMongoConfig `json:"mongoConfig,omitempty"`
	// PXCCfg is the same thing for a Galera member, read from its `Passing config to GCS`
	// line: the effective wsrep provider configuration, all ninety options of it. On an
	// operator-managed cluster it is the only place any of those numbers exists — cr.yaml
	// ships with no configuration section at all. See logsummary_pxcop_config.go.
	PXCCfg *lsPXCConfig `json:"pxcConfig,omitempty"`
	// PGPerf is the performance evidence a PostgreSQL server left in its own log:
	// checkpoints, sorts that spilled, slow statements, lock waits. Unlike the two above
	// it is not a configuration — PostgreSQL prints no configuration — it is the symptoms,
	// which is the only thing this engine gives an advisor to work with. See
	// logsummary_pgperf.go.
	PGPerf *lsPGPerf `json:"pgPerf,omitempty"`
}

// lsPhase is a stretch of time during which a source was in one state. Phases tile the
// whole bundle window without gaps, so "what was this node doing at t" is a lookup.
type lsPhase struct {
	Src     int     `json:"src"`
	From    float64 `json:"from"`
	To      float64 `json:"to"`
	State   string  `json:"state"`
	Sev     string  `json:"sev"`
	Members int     `json:"members,omitempty"`
	Primary string  `json:"primary,omitempty"` // yes | no | ""
	// Inferred marks a state the log did not state outright. See lsSeedState: a member
	// that was already SYNCED when the excerpt begins may log no transition at all, and
	// the UI has to be able to say "deduced" rather than "recorded".
	Inferred bool `json:"inferred,omitempty"`
}

// lsBucket is one cell of the swimlane: how many events of each severity a source
// produced in one slice of time.
type lsBucket struct {
	Src   int     `json:"src"`
	I     int     `json:"i"`
	TS    float64 `json:"ts"`
	OK    int     `json:"ok"`
	Warn  int     `json:"warn"`
	Bad   int     `json:"bad"`
	Info  int     `json:"info"`
	Count int     `json:"count"`
}

// lsStat is one label and how often it appeared, for the "what dominated this window"
// strip above the event list.
type lsStat struct {
	Label string `json:"label"`
	Class string `json:"class"`
	Sev   string `json:"sev"`
	Count int    `json:"count"`
}

// lsSummary is the headline for a whole bundle.
type lsSummary struct {
	Sources  int            `json:"sources"`
	Events   int            `json:"events"`
	FirstTS  float64        `json:"firstTs"`
	LastTS   float64        `json:"lastTs"`
	Counts   map[string]int `json:"counts"`  // severity → count
	Classes  map[string]int `json:"classes"` // class → count
	Top      []lsStat       `json:"top"`
	Overlap  float64        `json:"overlap"` // seconds every source has in common; 0 = none
	Disjoint bool           `json:"disjoint"`
}

// lsBundle is a set of logs read together.
type lsBundle struct {
	Sources []lsSource  `json:"sources"`
	Events  []lsEvent   `json:"events"`
	Phases  []lsPhase   `json:"phases"`
	Summary lsSummary   `json:"summary"`
	Finding []lsFinding `json:"findings"`
	// Names maps member UUIDs — in both the full and the abbreviated form Galera uses —
	// to node names, pooled across every source. One log names the members it can see; put
	// three together and almost every UUID in the bundle has a name attached to it, which
	// is the difference between "119e686d-8943 stopped answering" and "pxc02 stopped
	// answering".
	Names map[string]string `json:"names,omitempty"`
}

// lsInput is one log handed to the builder: its bytes and where they came from.
type lsInput struct {
	Name   string
	Path   string
	Origin string
	Engine string // "" = sniff it
	Data   []byte
	// Offset is added to every timestamp read from this source, in seconds — the manual
	// correction for host clock skew. Parsing gets the timezone right on its own; it can
	// do nothing about a machine whose clock is forty seconds fast.
	Offset float64
	// ReadAt is when this text was taken off the node, in epoch seconds; 0 for an upload,
	// where nothing knows.
	//
	// It exists because a log's last RECORD is not the end of what it covers. A healthy
	// PXC member writes nothing at all — measured elsewhere in this package: thirty
	// seconds of continuous inserts across three nodes produced zero records on all three
	// — so its file simply stops, hours before the moment you read it. Reading the stop as
	// the end of its coverage is what made five logs tailed from ONE cluster in ONE
	// request report that they did not overlap. See lsOverlap.
	ReadAt float64
}

// lsMaxEvents bounds a bundle. Past this the page is not the right tool: the classifier
// keeps roughly one event per twenty raw lines on a Galera log, so this is a few million
// lines of input.
const lsMaxEvents = 120000

// lsBuild parses, classifies and merges a set of logs into one bundle.
func lsBuild(inputs []lsInput) *lsBundle {
	b := &lsBundle{Sources: []lsSource{}, Events: []lsEvent{}, Phases: []lsPhase{},
		Names: map[string]string{}}
	for i, in := range inputs {
		src, events, names := lsBuildSource(i, in)
		b.Sources = append(b.Sources, src)
		b.Events = append(b.Events, events...)
		for k, v := range names {
			b.Names[k] = v
		}
	}
	// Merge in time order. Stable, and tie-broken by source then line, so two records
	// written in the same microsecond on two nodes keep a deterministic order instead of
	// shuffling between requests.
	sort.SliceStable(b.Events, func(i, j int) bool {
		if b.Events[i].TS != b.Events[j].TS {
			return b.Events[i].TS < b.Events[j].TS
		}
		if b.Events[i].Src != b.Events[j].Src {
			return b.Events[i].Src < b.Events[j].Src
		}
		return b.Events[i].Line < b.Events[j].Line
	})
	lsApplyNames(b)
	lsGRPromoteMembers(b)
	b.Events = lsCollapse(b.Events)
	if len(b.Events) > lsMaxEvents {
		b.Events = b.Events[:lsMaxEvents]
	}
	for i := range b.Events {
		b.Events[i].No = i + 1
	}
	b.Summary = lsSummarise(b)
	b.Phases = lsBuildPhases(b)
	b.Finding = lsFindings(b)
	return b
}

// lsApplyNames rewrites member UUIDs in event text as node names.
//
// It has to run here rather than in the classifier: one node's log usually names only some
// of the members, and the name for the UUID in front of you is very often in a DIFFERENT
// file. Pooling the maps first and rewriting afterwards is what lets "119e686d-8943 is no
// longer in the group" become "pxc02 is no longer in the group" — which is the whole
// reason to read three logs together rather than one at a time.
func lsApplyNames(b *lsBundle) {
	if len(b.Names) == 0 {
		return
	}
	rewrite := func(s string) string {
		for uuid, name := range b.Names {
			if name == "" || !strings.Contains(s, uuid) {
				continue
			}
			// The FIRST occurrence only, and the UUID stays beside the name.
			//
			// Keeping the UUID matters because it is what appears in the raw log, and a
			// reader checking the classifier against the file has to be able to find the
			// line. Rewriting only the first occurrence matters because Galera's internal
			// records repeat a UUID three or four times —
			// "evs::proto(<uuid>, OPERATIONAL, view_id(REG,<uuid>,3))" — and expanding
			// every one turns a terse line into an unreadable one.
			s = strings.Replace(s, uuid, name+" ("+uuid+")", 1)
		}
		return s
	}
	for i := range b.Events {
		e := &b.Events[i]
		e.Message = rewrite(e.Message)
		if e.Peer != "" {
			if n, ok := b.Names[e.Peer]; ok && n != "" {
				e.Peer = n
			}
		}
	}
}

// lsBuildSource reads one log into events.
func lsBuildSource(idx int, in lsInput) (lsSource, []lsEvent, map[string]string) {
	data := string(in.Data)
	src := lsSource{
		Idx: idx, Name: in.Name, Path: in.Path, Origin: in.Origin,
		Bytes: len(in.Data), Lines: strings.Count(data, "\n") + 1,
		Offset: in.Offset, ReadAt: in.ReadAt, Counts: map[string]int{},
	}
	if src.Origin == "" {
		src.Origin = "upload"
	}
	src.Engine = in.Engine
	if src.Engine == "" {
		src.Engine = lsSniffEngine(data)
	}

	var events []lsEvent
	names := map[string]string{}
	valkeySelf := ""
	switch src.Engine {
	case pktEngineMySQL:
		recs := lsFoldMySQL(data)
		src.Records = len(recs)
		src.Flavour = lsSniffFlavour(recs)
		src.Node = lsNodeName(recs)
		if src.Node == "" && src.Flavour == lsFlavourGroupRepl {
			src.Node = lsGRNodeName(recs)
		}
		names = lsUUIDNames(recs)
		if src.Flavour == lsFlavourGalera {
			src.PXCCfg = lsPXCScanConfig(recs)
		}
		for _, r := range recs {
			e, keep := lsClassifyMySQL(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
	case pktEnginePostgres:
		// PostgreSQL and Patroni are parsed here rather than by the shared classifier for
		// the same reason MongoDB is: a record is not a line. An ERROR is followed by its
		// DETAIL, its HINT and the STATEMENT that caused it, and a Patroni member's file is
		// two logs in two formats interleaved.
		recs := lsFoldPostgres(in.Data)
		src.Records = len(recs)
		src.Flavour = lsSniffPGFlavour(recs)
		src.Node = lsPGNodeName(recs)
		src.PGPerf = lsPGScanPerf(recs)
		for _, r := range recs {
			e, keep := lsClassifyPG(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
	case pktEngineMongoDB:
		// A replica-set member is parsed here rather than by the shared classifier. The
		// facts that matter — newState/oldState, hostAndPort, the rollback counts — live
		// in the record's `attr` object, and pktLogEntry does not carry it. A standalone
		// mongod has none of them and keeps the shared path below.
		// Every mongod log is parsed here, replica-set member or not. The sniff decides the
		// FLAVOUR — which findings may speak about this source — and nothing else.
		//
		// It used to decide the parse as well, and a log that failed the sniff fell through
		// to the shared classifier, which has no severity filter for MongoDB: twenty
		// thousand records became twenty thousand events, all of class other, and the
		// verdict layer read them as a broken asynchronous replica. lsClassifyMongo keeps
		// what the catalogue recognises plus anything the server itself called a warning,
		// which is the right filter for a standalone mongod too.
		recs := lsFoldMongo(in.Data)
		src.Records = len(recs)
		src.Node = lsMongoNodeName(recs)
		switch {
		case lsSniffMongos(recs):
			// Checked before the replica-set sniff, not after: a router logs plenty of
			// records that mention replica sets — it monitors every shard — and would
			// otherwise be filed as a member of one.
			src.Flavour = lsFlavourMongos
			src.Node = lsMongosNodeName(recs)
		case lsSniffMongoRS(recs):
			src.Flavour = lsFlavourMongoRS
		default:
			src.Flavour = src.Engine
		}
		for _, r := range recs {
			e, keep := lsClassifyMongo(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
		// A second pass for the one record the classifier deliberately drops. Slow
		// queries are the majority of a busy member's log and are noise one at a time;
		// summed they are the only per-collection, per-plan evidence in the file.
		src.MongoCfg = lsMongoScanConfig(recs)
		if st, worst := lsMongoScanSlow(recs); st.Ops > 0 || st.Debug > 0 {
			src.Mongo = &st
			for _, e := range worst {
				e.Src = idx
				events = append(events, e)
			}
		}
	case pktEngineK8sEvents:
		// Not a log at all: `kubectl get events -o json` is one List object, so it is
		// unmarshalled whole rather than folded line by line. It is here because the answer
		// to "why did that member restart" lives nowhere else — see logsummary_k8sevents.go.
		recs := lsFoldK8sEvents(in.Data)
		src.Records = len(recs)
		src.Flavour = lsFlavourK8sEvents
		src.Node = "kubernetes"
		for _, r := range recs {
			if e, keep := lsClassifyK8sEvent(r); keep {
				e.Src = idx
				events = append(events, e)
			}
		}
	case pktEngineOperator:
		// Not a database. A Kubernetes controller's log and its binlog collector's are
		// parsed here because the facts that matter are in a trailing JSON object with
		// duplicate keys, and because an ERROR record drags a Go stack trace behind it —
		// neither of which any line classifier in this package would survive. See
		// logsummary_pxcop.go.
		if in.Path == lsPathPITR || lsSniffOperator(data) == lsFlavourPXCPITR {
			recs := lsFoldPITR(data)
			src.Records = len(recs)
			src.Flavour = lsFlavourPXCPITR
			src.Node = lsPITRNodeName(recs)
			for _, r := range recs {
				e, keep := lsClassifyPITR(r)
				if !keep {
					continue
				}
				e.Src = idx
				events = append(events, e)
			}
			break
		}
		if in.Path == lsPathPBM || lsSniffPSMDB(data) == lsFlavourPBMAgent {
			recs := lsFoldPBM(data)
			src.Records = len(recs)
			src.Flavour = lsFlavourPBMAgent
			src.Node = lsPBMNodeName(recs)
			for _, r := range recs {
				e, keep := lsClassifyPBM(r)
				if !keep {
					continue
				}
				e.Src = idx
				events = append(events, e)
			}
			break
		}
		switch lsSniffOperatorFamily(data) {
		case lsFlavourCrunchyPGO:
			recs := lsFoldCrunchy(data)
			src.Records = len(recs)
			src.Flavour = lsFlavourCrunchyPGO
			src.Node = lsPGOpNodeName(recs, lsFlavourCrunchyPGO)
			for _, r := range recs {
				if e, keep := lsClassifyCrunchy(r); keep {
					e.Src = idx
					events = append(events, e)
				}
			}
			break
		case lsFlavourCNPG, lsFlavourCNPGManager:
			// A CNPG member's stream is TWO documents: the instance manager's own records
			// and PostgreSQL's, the latter wrapped as `{"logger":"postgres","record":{…}}`.
			// lsFoldCNPG splits them, and each half goes to the catalogue that owns it —
			// so a CNPG member is read by the same PostgreSQL rules as any other server.
			fl := lsSniffPGOperator(data)
			recs := lsFoldCNPG(data)
			src.Records = len(recs)
			src.Flavour = fl
			src.Node = lsPGOpNodeName(recs, fl)
			src.PGPerf = lsPGScanPerf(recs)
			for _, r := range recs {
				var e lsEvent
				var keep bool
				if r.Subsys == lsSubsysPostgres {
					e, keep = lsClassifyPG(r)
				} else {
					e, keep = lsClassifyCNPG(r)
				}
				if keep {
					e.Src = idx
					events = append(events, e)
				}
			}
			break
		case lsFlavourPSOperator:
			recs := lsFoldOperator(data)
			src.Records = len(recs)
			src.Flavour = lsFlavourPSOperator
			src.Node = lsPSOpNodeName(recs)
			for _, r := range recs {
				if e, keep := lsClassifyPSOperator(r); keep {
					e.Src = idx
					events = append(events, e)
				}
			}
			break
		case lsFlavourPerconaPG:
			recs := lsFoldOperator(data)
			src.Records = len(recs)
			src.Flavour = lsFlavourPerconaPG
			src.Node = lsPGOpNodeName(recs, lsFlavourPerconaPG)
			for _, r := range recs {
				if e, keep := lsClassifyPerconaPG(r); keep {
					e.Src = idx
					events = append(events, e)
				}
			}
			break
		}
		if src.Flavour != "" {
			break
		}
		// Both Percona operators are the same controller-runtime process writing the same
		// tab-separated lines, so the fold is shared and only the catalogue differs. The
		// controller group in the field object is what tells them apart, and it has to be
		// checked before the PXC one for the same reason the mongos sniff runs before the
		// replica-set sniff: nothing about the SHAPE of the line distinguishes them.
		recs := lsFoldOperator(data)
		src.Records = len(recs)
		if lsSniffPSMDB(data) == lsFlavourPSMDBOperator {
			src.Flavour = lsFlavourPSMDBOperator
			src.Node = lsPSMDBNodeName(recs)
			for _, r := range recs {
				e, keep := lsClassifyPSMDBOperator(r)
				if !keep {
					continue
				}
				e.Src = idx
				events = append(events, e)
			}
			break
		}
		src.Flavour = lsFlavourPXCOperator
		src.Node = lsOpNodeName(recs)
		for _, r := range recs {
			e, keep := lsClassifyOperator(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
	case pktEngineValkey:
		// Valkey is parsed here rather than by the shared classifier for a reason the other
		// engines do not have: its source is two logs in one file. dbcanvas sets no
		// `logfile`, so the collector reads the journal — and the journal holds systemd's
		// records beside Valkey's. That is not noise to be dropped. A SIGKILLed
		// valkey-server writes NOTHING, so systemd's "code=killed, status=9/KILL" is the
		// only evidence in existence that the process was killed rather than stopped.
		recs, host := lsFoldValkey(in.Data)
		src.Records = len(recs)
		src.Flavour = lsSniffValkeyFlavour(recs)
		src.Node = lsValkeyNodeName(host)
		// Held until the node's name is settled below, because the name may still come from
		// the file name — a bare stdout log has no host in it to read.
		valkeySelf = lsValkeySelfID(recs)
		for _, r := range recs {
			e, keep := lsClassifyValkey(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
	default:
		// The other engines already have line classifiers, written for the Packet
		// Inspector's correlation pane. They are reused verbatim rather than reimplemented:
		// what the Log Summary adds for them is the shared timeline, the severity split and
		// the multi-source comparison, not a second parse of the same formats.
		entries := pktParseServerLog(in.Data, src.Engine)
		src.Records = len(entries)
		src.Flavour = src.Engine
		for i, en := range entries {
			e := lsFromPktEntry(en, i+1)
			e.Src = idx
			events = append(events, e)
		}
	}

	lsResolveSelf(src.Node, events)
	switch src.Flavour {
	case lsFlavourGalera:
		// Galera's own records carry the state; nothing to resolve.
	case lsFlavourGroupRepl:
		lsResolveGroupRepl(events)
	case lsFlavourMongoRS:
		lsResolveMongo(src.Node, events)
	case lsFlavourMongos:
		lsResolveMongos(events)
	case lsFlavourPostgres, lsFlavourPGStream, lsFlavourPatroni:
		lsResolvePG(events)
	case lsFlavourPXCOperator:
		lsResolveOperator(events)
	case lsFlavourPXCPITR:
		lsResolvePITRCollector(events)
	case lsFlavourCNPG, lsFlavourCNPGManager:
		lsResolveCNPG(events)
	case lsFlavourPerconaPG, lsFlavourCrunchyPGO:
		lsResolvePGOperator(events)
	case lsFlavourPSOperator:
		lsResolvePSOperator(events)
	case lsFlavourK8sEvents:
		lsResolveK8sEvents(events)
	case lsFlavourPSMDBOperator:
		lsResolvePSMDBOperator(events)
	case lsFlavourPBMAgent:
		lsResolvePBMAgent(events)
	case lsFlavourValkey, lsFlavourValkeyRepl, lsFlavourValkeyCluster:
		// The flavour is passed in because it decides one word: a lone server with no
		// replication anywhere in its file is RUNNING, not PRIMARY. There is nothing for it
		// to be primary of, and the word would imply a topology it is not in.
		lsResolveValkey(src.Flavour, events)
	default:
		lsResolveStandalone(events)
	}
	for i := range events {
		if events[i].TS == 0 {
			continue
		}
		if in.Offset != 0 {
			events[i].TS += in.Offset
			if events[i].EndTS > 0 {
				events[i].EndTS += in.Offset
			}
		}
		if src.FirstTS == 0 || events[i].TS < src.FirstTS {
			src.FirstTS = events[i].TS
		}
		if events[i].TS > src.LastTS {
			src.LastTS = events[i].TS
		}
		if events[i].Approx {
			src.Untimed++
		}
		src.Counts[events[i].Sev]++
	}
	src.Events = len(events)
	if src.Node == "" {
		src.Node = lsNodeFromName(in.Name)
	}
	// A Valkey Cluster node's own id, paired with whatever name it ended up with. Registered
	// here rather than in the switch because the name may have come from the file name a
	// moment ago, and a name is the whole point of the pairing.
	if valkeySelf != "" && src.Node != "" {
		names[valkeySelf] = src.Node
	}
	return src, events, names
}

// lsResolveSelf turns the records in which a node names ITSELF into state evidence.
//
// Galera reports every member's progress to every member, so "Member 1.0 (pxc02) synced
// with group" appears in all three logs — and in pxc02's own it is a statement about
// pxc02. That distinction cannot be made inside the classifier, which sees one record at a
// time and does not know whose file it is in; here the source's own name is known.
//
// It matters because a log fragment can contain no transition at all. In the network-
// partition fixture pxc02 was SYNCED throughout and logged not one `Shifting` line, so
// without this it would sit at UNKNOWN for the whole incident — and the majority that
// stayed up would show as one node rather than two.
func lsResolveSelf(node string, events []lsEvent) {
	if node == "" {
		return
	}
	for i := range events {
		e := &events[i]
		if e.State != "" || e.Peer != node {
			continue
		}
		switch e.Label {
		case "Member synced with group":
			e.State = lsStateSynced
			e.Meaning = "This node reached SYNCED and is serving queries."
		case "Member desynced itself":
			e.State = lsStateDonor
		}
	}
}

// lsResolveStandalone gives a non-cluster server the only two states it has.
//
// A standalone MySQL or an asynchronous replica has no wsrep state machine, so the Galera
// vocabulary means nothing on it — and left to the shared phase builder every such node
// sat in CLOSED from its first start-up record onward, because nothing ever moved it out.
// A live three-node replication topology, entirely healthy, was reported as three servers
// that had not served a query in thirteen minutes.
//
// For these nodes the question is simply whether mysqld is up: `ready for connections` is
// the line that means yes, and a shutdown or a crash is what means no.
func lsResolveStandalone(events []lsEvent) {
	for i := range events {
		e := &events[i]
		switch {
		case e.Class == lsClassCrash && e.Sev == lsSevBad:
			e.State = lsStateDown
		case strings.HasPrefix(e.Label, "Server ready for connections"):
			e.State = lsStateUp
		case strings.HasPrefix(e.Label, "Server starting"):
			e.State = lsStateStarting
		case e.Label == "Shutdown complete":
			e.State = lsStateDown
		case e.Label == "Shutdown requested":
			e.State = lsStateStarting // stopping: up, but on its way out of service
		}
	}
}

// lsResolveGroupRepl is lsResolveStandalone's Group Replication counterpart, and differs
// from it in exactly one place that matters.
//
// A standalone server that reaches `ready for connections` is RUNNING — up and serving,
// which is the whole of what is being asked of it. A Group Replication member that reaches
// the same line is OFFLINE: mysqld is up, but the plugin is not, and nothing in the
// cluster's data will reach it. The corpus contains the case that makes this worth
// separating (g04-crash-kill9): systemd restarted a SIGKILLed member, mysqld came back and
// logged `ready for connections`, group_replication_start_on_boot was OFF so nothing
// rejoined, and the log said nothing further. Measured at that moment the server was
// writable and 666 transactions behind the group. Calling that RUNNING would have painted
// the lane green for the rest of the window.
//
// The plugin's own records move it on from there — MY-013587 to RECOVERING, MY-011490 to
// ONLINE — so a healthy start shows a brief OFFLINE stripe before it joins, which is
// exactly what was true.
func lsResolveGroupRepl(events []lsEvent) {
	for i := range events {
		e := &events[i]
		if e.State != "" {
			continue // a plugin record already said what state this is
		}
		switch {
		case e.Class == lsClassCrash && e.Sev == lsSevBad:
			e.State = lsStateDown
		case strings.HasPrefix(e.Label, "Server ready for connections"):
			e.State = lsStateOffline
		case strings.HasPrefix(e.Label, "Server starting"):
			e.State = lsStateStarting
		case e.Label == "Shutdown complete":
			e.State = lsStateDown
		case e.Label == "Shutdown requested":
			e.State = lsStateStarting
		}
	}
}

// lsNodeFromName falls back to the file name for a node's identity. An uploaded pxc02.err
// is named after the node far more often than not, and a label is better than a blank.
func lsNodeFromName(name string) string {
	base := name
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".err", ".log", ".txt", ".gz"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// lsFromPktEntry adapts a Packet-Inspector log entry to a Log Summary event.
//
// The severity mapping is the interesting part: those classifiers were written to answer
// "which of these records could explain the packets I am looking at", so everything they
// recognise is a problem of some kind and everything they do not is `other`. That maps
// cleanly onto warn/bad, and leaves `ok` unused for those engines until they get a
// catalogue of their own like Galera's.
func lsFromPktEntry(en pktLogEntry, line int) lsEvent {
	sev := lsSevInfo
	class := lsClassOther
	switch en.Class {
	case pktLogAbort:
		sev, class = lsSevWarn, lsClassClient
	case pktLogAuth:
		sev, class = lsSevWarn, lsClassSecurity
	case pktLogDNS:
		sev, class = lsSevWarn, lsClassNetwork
	case pktLogListen:
		sev, class = lsSevBad, lsClassStartup
	case pktLogTLS:
		sev, class = lsSevWarn, lsClassSecurity
	case pktLogRepl:
		sev, class = lsSevBad, lsClassReplica
	case pktLogLifecycle:
		sev, class = lsSevWarn, lsClassStartup
		if strings.Contains(en.Label, "startup") || strings.Contains(en.Message, "ready for connections") {
			sev = lsSevOK
		}
	case pktLogCluster:
		sev, class = lsSevWarn, lsClassMember
	}
	sev = lsWorse(sev, lsLevelFloor(en.Level))
	return lsEvent{
		TS: en.TS, Line: line, Time: en.Time, Level: en.Level, Code: en.Code,
		Subsys: en.Subsys, Class: class, Sev: sev,
		Label: en.Label, Message: en.Message, Detail: en.Reason,
	}
}

// ---------------------------------------------------------------- summary

func lsSummarise(b *lsBundle) lsSummary {
	s := lsSummary{
		Sources: len(b.Sources), Events: len(b.Events),
		Counts: map[string]int{}, Classes: map[string]int{}, Top: []lsStat{},
	}
	type key struct{ label, class, sev string }
	counts := map[key]int{}
	for _, e := range b.Events {
		n := 1
		if e.Repeat > 1 {
			n = e.Repeat
		}
		s.Counts[e.Sev] += n
		s.Classes[e.Class] += n
		if e.Sev != lsSevInfo {
			counts[key{e.Label, e.Class, e.Sev}] += n
		}
		if e.TS > 0 {
			if s.FirstTS == 0 || e.TS < s.FirstTS {
				s.FirstTS = e.TS
			}
			if end := lsEndOf(e); end > s.LastTS {
				s.LastTS = end
			}
		}
	}
	for k, n := range counts {
		s.Top = append(s.Top, lsStat{Label: k.label, Class: k.class, Sev: k.sev, Count: n})
	}
	sort.Slice(s.Top, func(i, j int) bool {
		// Worst first, then most frequent: "one node crashed" must outrank "412 peer
		// timeouts", because it is the thing that caused them.
		if lsSevRank[s.Top[i].Sev] != lsSevRank[s.Top[j].Sev] {
			return lsSevRank[s.Top[i].Sev] > lsSevRank[s.Top[j].Sev]
		}
		if s.Top[i].Count != s.Top[j].Count {
			return s.Top[i].Count > s.Top[j].Count
		}
		return s.Top[i].Label < s.Top[j].Label
	})
	if len(s.Top) > 24 {
		s.Top = s.Top[:24]
	}
	s.Overlap, s.Disjoint = lsOverlap(b.Sources)
	return s
}

// lsOverlap is how much time every source has in common — the window in which a
// comparison between them means anything.
//
// Disjoint is the case worth naming: logs from different days, or from a node whose log
// was rotated before the incident. Two files that never overlap will still draw a
// perfectly plausible-looking timeline, and it will be a lie.
func lsOverlap(sources []lsSource) (float64, bool) {
	lo, hi := 0.0, 0.0
	n := 0
	for _, s := range sources {
		if s.FirstTS == 0 {
			continue
		}
		n++
		if lo == 0 || s.FirstTS > lo {
			lo = s.FirstTS
		}
		if e := lsCoverEnd(s); hi == 0 || e < hi {
			hi = e
		}
	}
	if n < 2 {
		return 0, false
	}
	if hi <= lo {
		return 0, true
	}
	return hi - lo, false
}

// lsCoverEnd is the last instant a source has anything to say ABOUT, which is not the same
// as the last instant it said anything.
//
// A log that stops is a server that carried on and had nothing to report — the assumption
// lsBuildPhases has always made when it runs the final phase to the end of the window, and
// the one that makes silence readable as the good news it usually is. lsOverlap did not
// make it, and the contradiction only became visible with a Kubernetes bundle: three
// healthy PXC members, tailed at 08:17, whose last record was the 06:14 line where they
// finished starting, beside a binlog collector whose pod had restarted at 06:23 and whose
// log therefore begins there. Latest start after earliest last-record, so five logs read
// from one cluster in one request were reported as not overlapping at all.
//
// The read instant is the honest end, and it exists only for logs tailed from a node. An
// uploaded file keeps the old behaviour, because nothing knows when it was cut: its last
// record is genuinely all the evidence there is about how far it reaches.
func lsCoverEnd(s lsSource) float64 {
	if s.ReadAt > s.LastTS {
		return s.ReadAt
	}
	return s.LastTS
}

// ---------------------------------------------------------------- phases

// lsBuildPhases turns each source's state-bearing events into a continuous track.
//
// Every phase runs to the start of the next one, and the last runs to the end of the
// bundle window — a log that simply stops is a node that carried on and had nothing to
// say, which on a database server is the definition of a good day. The exception is a
// track that ends in DOWN, which stays down.
func lsBuildPhases(b *lsBundle) []lsPhase {
	out := []lsPhase{}
	if len(b.Sources) == 0 {
		return out
	}
	start, end := b.Summary.FirstTS, b.Summary.LastTS
	if end <= start {
		end = start + 1
	}
	for _, src := range b.Sources {
		seed, inferred := lsSeedState(b, src.Idx)
		var track []lsPhase
		from := start
		// A DEDUCED seed may not be painted over time the source did not cover.
		//
		// lsSeedState's last resort is "a server writing to its log is running, and the
		// log is the evidence" — which is sound, and only for the stretch the log actually
		// spans. Applied from the bundle's start it invents the rest: a mongod whose
		// 5,000-line tail covers eighteen minutes of a two-and-a-half-hour bundle was
		// drawn as SERVING for the whole window, in green, on the strength of records that
		// begin two hours later. Reported by a reader who uploaded the same members' full
		// logs and got a different — correct, and much less green — picture.
		//
		// So the lead-in is UNKNOWN, which is what the source says about it, and the
		// deduction starts where the evidence does. A STATED seed is left alone: the
		// left-hand side of a first transition ("Shifting SYNCED -> DONOR") is a real
		// statement about the moment before the record, not a deduction from its
		// existence.
		//
		// The asymmetry with the END of a track is deliberate. A log that stops is a
		// server that carried on and had nothing to say; a log that starts late is a
		// server this bundle knows nothing about until it does. See lsCoverEnd for the
		// other half of the same idea.
		if inferred && src.FirstTS > start {
			track = append(track, lsPhase{
				Src: src.Idx, From: start, To: src.FirstTS,
				State: "UNKNOWN", Sev: lsSevInfo,
			})
			from = src.FirstTS
		}
		cur := lsPhase{Src: src.Idx, From: from, State: seed, Sev: lsStateSev(seed), Inferred: inferred}
		if seed == "UNKNOWN" {
			cur.Sev = lsSevInfo
		}
		for _, e := range b.Events {
			if e.Src != src.Idx || e.TS <= 0 {
				continue
			}
			next := cur
			next.From = e.TS
			// A server start wipes the slate: whatever state the previous run ended in
			// says nothing about this one, and neither does the membership it last saw.
			if e.Class == lsClassStartup && e.State == "" && strings.HasPrefix(e.Label, "Server starting") {
				next.State, next.Members, next.Primary = lsStateClosed, 0, ""
			} else {
				// Membership and primary-ness are part of the phase, not separate tracks:
				// "SYNCED, 2 members, primary" and "SYNCED, 3 members, primary" are two
				// different answers to "what was this node doing", and a reader asking
				// about an instant wants the one that was true THEN. Folding them into one
				// stripe would date-stamp the phase with whichever value happened last.
				if e.Members > 0 && (e.Class == lsClassQuorum || e.Class == lsClassMember) {
					next.Members = e.Members
				}
				if e.Primary != "" {
					next.Primary = e.Primary
				}
				if e.State != "" {
					next.State, next.Inferred = e.State, false
				}
				if next.State == lsStateDown || next.State == lsStateClosed {
					next.Members, next.Primary = 0, ""
				}
			}
			if next.State == cur.State && next.Members == cur.Members && next.Primary == cur.Primary {
				continue
			}
			next.Sev = lsStateSev(next.State)
			track = lsPushPhase(track, cur, e.TS)
			cur = next
		}
		cur.To = end
		track = append(track, cur)
		out = append(out, track...)
	}
	return out
}

// lsSeedState works out what state a source was in before its log says anything, and
// whether that answer was stated or deduced.
//
// A log is almost always a fragment, and this matters more than it sounds. A node that was
// already SYNCED when the excerpt begins never logs a transition INTO SYNCED — so without
// some answer here, the two members that stayed up through a partition both read as
// "unknown", and the one question the page exists to answer has no answer for exactly the
// nodes that were fine.
//
// Two answers, in order of how much they can be trusted:
//
//  1. Stated. The left-hand side of the first transition OUT of a state is not a guess:
//     `Shifting SYNCED -> DONOR/DESYNCED` says outright what the node was doing a moment
//     earlier.
//
//  2. Deduced, and flagged as such. A member that logs no state transition at ALL did not
//     change state during the window — every transient state (JOINER, JOINED, DONOR,
//     PRIMARY) necessarily ends, and would have logged its end. If such a node also
//     reports itself inside a primary component, SYNCED is the only state left. That is
//     sound, but it is reasoning rather than reading, so it is marked Inferred and the UI
//     says so rather than presenting it as something the file stated.
func lsSeedState(b *lsBundle, src int) (string, bool) {
	sawPrimary, sawAny := false, false
	for _, e := range b.Events {
		if e.Src != src {
			continue
		}
		sawAny = true
		// A start-up in the excerpt means the node was not in the cluster before it.
		if e.Class == lsClassStartup && strings.HasPrefix(e.Label, "Server starting") {
			return lsStateDown, false
		}
		if e.From != "" {
			return e.From, false
		}
		if e.State != "" {
			// A transition with no stated origin (a bare "Synced and serving") says
			// nothing about what came before it.
			return "UNKNOWN", false
		}
		if e.Primary == "yes" {
			sawPrimary = true
		}
	}
	if sawAny && sawPrimary {
		return lsStateSynced, true
	}
	// A server with no cluster records at all that is writing to its log is running: the
	// log is the evidence. Deduced, and flagged as such.
	//
	// It does not apply to a Kubernetes Events feed, which is the one source here that is
	// not a process at all. "This was running" is a deduction about the writer of a log,
	// and nothing wrote these — they are API objects that the collector asked for. An
	// operator IS a process writing its own log, so the deduction stands for those.
	for _, s := range b.Sources {
		if s.Idx == src && s.Flavour != lsFlavourGalera && s.Events > 0 &&
			s.Engine != pktEngineK8sEvents {
			return lsStateUp, true
		}
	}
	return "UNKNOWN", false
}

// lsPushPhase closes a phase at t and appends it, dropping zero-length ones — a node can
// pass through PRIMARY and JOINER in the same microsecond and neither is worth a stripe.
func lsPushPhase(track []lsPhase, p lsPhase, t float64) []lsPhase {
	p.To = t
	if p.To <= p.From {
		return track
	}
	return append(track, p)
}

// lsSettledMS is the shortest a phase can be and still count as a state the node was IN,
// rather than a transition it was passing THROUGH.
//
// Galera walks several states in the same microsecond. Cutting a member off produced, in
// order and within 370 µs of each other: a view saying "1 member, non-primary" while the
// node was still nominally SYNCED, then NON-PRIMARY, then `Shifting SYNCED -> OPEN`. All
// three are real records and the phases between them are real, but a readout that lands in
// the first of them reports "SYNCED, 1 member, non-primary" — three facts that were
// momentarily all true and together describe nothing. Fifty milliseconds is far longer
// than any of those slivers and far shorter than any state worth reporting.
const lsSettledMS = 0.050

// lsStateAt answers "what was this source doing at t" from the phase track, skipping past
// the transitional slivers. Use this for anything a human reads; use lsPhaseAt when the
// literal phase covering an instant is what is wanted.
func lsStateAt(phases []lsPhase, src int, t float64) (lsPhase, bool) {
	p, ok := lsPhaseAt(phases, src, t)
	if !ok || p.To-p.From >= lsSettledMS {
		return p, ok
	}
	// Walk forward to the first phase that lasted long enough to mean something.
	best, found := p, false
	for _, q := range phases {
		if q.Src != src || q.From < p.From || q.To-q.From < lsSettledMS {
			continue
		}
		if !found || q.From < best.From {
			best, found = q, true
		}
	}
	if found {
		return best, true
	}
	return p, ok
}

// lsPhaseAt returns the phase literally covering t.
func lsPhaseAt(phases []lsPhase, src int, t float64) (lsPhase, bool) {
	for _, p := range phases {
		if p.Src == src && t >= p.From && t < p.To {
			return p, true
		}
	}
	// Past the end of the track, the last phase still applies.
	var last lsPhase
	found := false
	for _, p := range phases {
		if p.Src == src && (!found || p.To > last.To) {
			last, found = p, true
		}
	}
	return last, found
}

// ---------------------------------------------------------------- bucketing

// lsBucketise counts events per source over a time grid. The server does this so the
// browser never holds more than one page of a bundle that may be a hundred thousand
// events, exactly as the Packet Inspector's timeline does.
func lsBucketise(events []lsEvent, sources []lsSource, from, to float64, n int) []lsBucket {
	if n < 2 {
		n = 2
	}
	if to <= from {
		to = from + 1
	}
	width := (to - from) / float64(n)
	out := make([]lsBucket, 0, n*len(sources))
	index := map[[2]int]int{}
	for _, s := range sources {
		for i := 0; i < n; i++ {
			index[[2]int{s.Idx, i}] = len(out)
			out = append(out, lsBucket{Src: s.Idx, I: i, TS: from + float64(i)*width})
		}
	}
	for _, e := range events {
		if e.TS < from || e.TS > to {
			continue
		}
		i := int((e.TS - from) / width)
		if i >= n {
			i = n - 1
		}
		if i < 0 {
			i = 0
		}
		at, ok := index[[2]int{e.Src, i}]
		if !ok {
			continue
		}
		c := 1
		if e.Repeat > 1 {
			c = e.Repeat
		}
		b := &out[at]
		b.Count += c
		switch e.Sev {
		case lsSevOK:
			b.OK += c
		case lsSevWarn:
			b.Warn += c
		case lsSevBad:
			b.Bad += c
		default:
			b.Info += c
		}
	}
	return out
}
