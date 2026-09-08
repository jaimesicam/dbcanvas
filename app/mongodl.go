package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// mongodl.go — the thing you always end up wanting off a MongoDB node, as one
// download: its diagnostic.data together with the log that explains it.
//
// Both halves were already reachable — the File Manager browses any path on a
// node, and the FTDC Summary page reads and charts a capture in place — but both
// took several steps and knowing where to look, and neither gives you the files to
// attach to a ticket or hand to somebody. So it is one entry on the node's
// right-click menu, and the paths are worked out HERE rather than in the browser:
// they come from mongoDataDir/mongoLogDir and ftdcDiagDirs, and a constant
// duplicated into JavaScript is a constant that drifts.
//
// One download rather than two because they are never useful apart: FTDC says what
// the server was doing and the log says what happened to it, and whoever reads them
// wants them from the same node over the same hours. They arrive under one
// directory named after the node — <hostname>_log_diagnostic_data — so unpacking
// three members' bundles side by side leaves three named directories rather than
// three collisions.
//
// The download itself is streamNodeArchive (nodefs.go), the same code the File
// Manager uses, with a root directory added. Nothing is buffered, which matters
// here more than anywhere else in this app — a busy server's log runs to gigabytes,
// and a long-lived node's diagnostic.data is hundreds of files.

// mongoLogPath is where this app's own provisioning puts a member's log. A mongos
// writes mongos.log (mongosConfYAML) and everything else mongod.log
// (mongodConfYAML), which is the only difference between the two.
func mongoLogPath(role string) string {
	if role == "mongos" {
		return mongoLogDir + "/mongos.log"
	}
	return mongoLogDir + "/mongod.log"
}

// mongoBundleName is the download's name, and the directory inside it: the node's
// own hostname, so the archive identifies the machine it came off without anyone
// having to rename it. The suffix spells out the contents because these end up in
// a ticket alongside captures from other tools.
func mongoBundleName(host string) string {
	h := sanitizeName(host)
	if h == "" {
		h = "mongodb"
	}
	return h + "_log_diagnostic_data"
}

// mongoDiagDirOn finds the node's diagnostic.data. It probes rather than assumes:
// the directory this app's own layout produces is the first candidate, but the same
// list (ftdcDiagDirs) covers the upstream/Debian and container layouts, because a
// Linux Client with somebody else's data mounted on it is a real case here.
//
// The mongos entry is the one that surprises people: mongos derives its FTDC
// directory from its LOG path, not its dbPath, because it has no dbPath.
func (a *App) mongoDiagDirOn(ctx context.Context, containerID string) (string, error) {
	script := "for d in " + strings.Join(ftdcDiagDirs, " ") +
		"; do if [ -d \"$d\" ]; then echo \"$d\"; exit 0; fi; done; exit 3"
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", script}, nil)
	if err != nil {
		return "", fmt.Errorf("look for diagnostic.data: %w", err)
	}
	dir := strings.TrimSpace(res.Stdout)
	if res.Code != 0 || dir == "" {
		return "", fmt.Errorf("no diagnostic.data on this node — looked in %s", strings.Join(ftdcDiagDirs, ", "))
	}
	return dir, nil
}

// loadRunningMongoNode resolves a request's node as a RUNNING MongoDB one, with its
// design entry and its hostname — the role, and the name the bundle is called
// after, which loadRunningNode does not carry.
//
// It leans on loadRunningNode for every guard that matters (the stack is yours, the
// node is deployed, it is running, its container id is re-resolved if it drifted)
// and only adds the design lookup, so there is one implementation of "may this
// caller touch this node" rather than two.
func (a *App) loadRunningMongoNode(w http.ResponseWriter, r *http.Request) (Deployment, designNode, string, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return Deployment{}, designNode{}, "", false
	}
	st, err := a.store.GetStack(dep.StackID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read stack")
		return Deployment{}, designNode{}, "", false
	}
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	for _, n := range doc.Nodes {
		if n.ID == dep.NodeID {
			if !isMongoNodeType(n.Type) {
				writeErr(w, http.StatusBadRequest, "that node is not a MongoDB node")
				return Deployment{}, designNode{}, "", false
			}
			// The hostname the node actually answers to, not its label: the same
			// name that is in the log's own header lines and in every FQDN the
			// stack's members use for each other.
			host := stackHostnames(doc)[n.ID]
			if host == "" {
				host = n.Label
			}
			return dep, n, host, true
		}
	}
	writeErr(w, http.StatusNotFound, "node not found in this stack's design")
	return Deployment{}, designNode{}, "", false
}

// handleMongoDiagDownload streams a MongoDB node's diagnostic.data and its log as
// one .tar.gz, under a single <hostname>_log_diagnostic_data directory.
//
// The whole FTDC directory, not a selection of it: those files are only useful as a
// run, and the reader on the other end (this app's FTDC Summary, Big Hole, mongod's
// own tooling) wants the directory it was written as.
//
// A node with no diagnostic.data yet — a mongos that has only just started, or a
// container whose FTDC is somewhere ftdcDiagDirs does not know about — still gets
// the log rather than a 404, because the log is the half that is always there and
// refusing the download would leave no way to get it from this menu.
func (a *App) handleMongoDiagDownload(w http.ResponseWriter, r *http.Request) {
	dep, node, host, ok := a.loadRunningMongoNode(w, r)
	if !ok {
		return
	}
	paths := []string{mongoLogPath(node.Role)}
	if dir, err := a.mongoDiagDirOn(r.Context(), dep.ContainerID); err == nil {
		paths = append(paths, dir)
	}
	name := mongoBundleName(host)
	a.streamNodeArchive(w, r, dep, paths, name, name)
}

// isMongoNodeType is the three shapes a MongoDB node comes in: a standalone, a
// replica-set member, and a member of a sharded cluster (which includes the mongos
// router — it has a log, and FTDC of its own).
func isMongoNodeType(t string) bool {
	return t == "psm" || t == "psmrs" || t == "psmdb"
}
