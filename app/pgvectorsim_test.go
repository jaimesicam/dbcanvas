package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// pgvDoc links a pgvectorsim node to one target, a node or a frame, plus any extras.
func pgvDoc(targetID string, nodes []designNode, frames []designFrame, extra ...designEdge) designDoc {
	doc := designDoc{Nodes: append([]designNode{{ID: "sim", Type: "pgvectorsim", Label: "pgvectorsim-01"}}, nodes...), Frames: frames}
	e := designEdge{ID: "e"}
	e.From.Node, e.To.Node = targetID, "sim"
	doc.Edges = append([]designEdge{e}, extra...)
	return doc
}

func edgeBetween(id, from, to string) designEdge {
	e := designEdge{ID: id}
	e.From.Node, e.To.Node = from, to
	return e
}

var pgvSim = designNode{ID: "sim", Type: "pgvectorsim", Label: "pgvectorsim-01"}

func TestPGVectorSimIssues(t *testing.T) {
	// Unlinked: an error.
	if iss := pgVectorSimIssues(designDoc{Nodes: []designNode{pgvSim}}, pgvSim); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("unlinked: %v", iss)
	}

	// A standalone node with pgvector: clean. Without it: the warning, not an error.
	pg := designNode{ID: "p", Type: "pg", Label: "pg-01", PGVector: true}
	if iss := pgVectorSimIssues(pgvDoc("p", []designNode{pg}, nil), pgvSim); len(iss) != 0 {
		t.Errorf("pg with pgvector: %v", iss)
	}
	pg.PGVector = false
	iss := pgVectorSimIssues(pgvDoc("p", []designNode{pg}, nil), pgvSim)
	if len(iss) != 1 || iss[0].Level != "warning" ||
		!strings.Contains(iss[0].Message, "turn on pgvector on PostgreSQL node pg-01 to use HNSW indexes") ||
		!strings.Contains(iss[0].Message, "real[]") {
		t.Errorf("pg without pgvector: %v", iss)
	}

	// Cluster frames.
	for _, typ := range []string{"patroni", "repmgr", "spock"} {
		f := designFrame{ID: "f", Type: typ, Label: "c", PGVector: true}
		if iss := pgVectorSimIssues(pgvDoc("f", nil, []designFrame{f}), pgvSim); len(iss) != 0 {
			t.Errorf("%s with pgvector: %v", typ, iss)
		}
	}

	// HAProxy in front of Patroni: the backend's option counts.
	pat := designFrame{ID: "f", Type: "patroni", Label: "pat"}
	hap := designNode{ID: "h", Type: "haproxy", Label: "haproxy-01"}
	iss = pgVectorSimIssues(pgvDoc("h", []designNode{hap}, []designFrame{pat}, edgeBetween("e2", "f", "h")), pgvSim)
	if len(iss) != 1 || iss[0].Level != "warning" || !strings.Contains(iss[0].Message, "cluster pat") {
		t.Errorf("haproxy→patroni without pgvector: %v", iss)
	}
	// HAProxy in front of PXC: an error.
	pxc := designFrame{ID: "f", Type: "pxc", Label: "pxc"}
	if iss := pgVectorSimIssues(pgvDoc("h", []designNode{hap}, []designFrame{pxc}, edgeBetween("e2", "f", "h")), pgvSim); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("haproxy→pxc: %v", iss)
	}

	// PgBouncer in front of a standalone node with pgvector: clean.
	pg.PGVector = true
	pgb := designNode{ID: "b", Type: "pgbouncer", Label: "pgbouncer-01"}
	if iss := pgVectorSimIssues(pgvDoc("b", []designNode{pgb, pg}, nil, edgeBetween("e2", "p", "b")), pgvSim); len(iss) != 0 {
		t.Errorf("pgbouncer→pg: %v", iss)
	}
	// ...and with no backend: an error.
	if iss := pgVectorSimIssues(pgvDoc("b", []designNode{pgb}, nil), pgvSim); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("pgbouncer unlinked: %v", iss)
	}

	// K3D: the Percona PG operator with pgvector and a LoadBalancer is clean; a MongoDB
	// operator is an error; CloudNativePG and Crunchy PGO are clean with pgvector on and warn
	// without it.
	k := designFrame{ID: "k", Type: "k3d", Label: "k3d-00", K3DOperator: "pg", K3DPgVector: true, K3DExposePG: "loadbalancer"}
	if iss := pgVectorSimIssues(pgvDoc("k", nil, []designFrame{k}), pgvSim); len(iss) != 0 {
		t.Errorf("k3d pg: %v", iss)
	}
	k.K3DOperator = "psmdb"
	if iss := pgVectorSimIssues(pgvDoc("k", nil, []designFrame{k}), pgvSim); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("k3d psmdb: %v", iss)
	}
	k.K3DOperator, k.K3DCNPGExpose = "cnpg", "loadbalancer"
	if iss := pgVectorSimIssues(pgvDoc("k", nil, []designFrame{k}), pgvSim); len(iss) != 0 {
		t.Errorf("k3d cnpg with pgvector: %v", iss)
	}
	k.K3DPgVector = false
	if iss := pgVectorSimIssues(pgvDoc("k", nil, []designFrame{k}), pgvSim); len(iss) != 1 || iss[0].Level != "warning" || !strings.Contains(iss[0].Message, "turn on pgvector") {
		t.Errorf("k3d cnpg without pgvector: %v", iss)
	}
	k.K3DOperator, k.K3DPgVector, k.K3DExposePGBouncer = "pgo", true, "loadbalancer"
	if iss := pgVectorSimIssues(pgvDoc("k", nil, []designFrame{k}), pgvSim); len(iss) != 0 {
		t.Errorf("k3d pgo with pgvector: %v", iss)
	}
}

// The builtin template validates down to no pgvectorsim issue at all.
func TestPGVectorSimTemplate(t *testing.T) {
	tpl, ok := findBuiltinTemplate(builtinPrefix + "pg-pgvector-support-sim")
	if !ok {
		t.Fatal("template missing")
	}
	var doc designDoc
	if err := json.Unmarshal(tpl.Design, &doc); err != nil {
		t.Fatal(err)
	}
	sim := nodeByID(doc, "tpl-pgvectorsim")
	if iss := pgVectorSimIssues(doc, sim); len(iss) != 0 {
		t.Errorf("template: %v", iss)
	}
}
