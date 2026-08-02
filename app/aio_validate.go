package main

import (
	"fmt"
	"strings"
)

// aio_validate.go — the rules that stop an All-in-One design from reaching dnf.
//
// The designer already greys out impossible choices, but the form is not the
// authority: a stack saved before a rule existed, or edited through the API,
// must still be refused. Every rule here therefore duplicates a UI affordance on
// purpose.
//
// The load-bearing one is the MySQL flavor conflict. percona-server-server and
// percona-xtradb-cluster-server both Provides: mysql-server, so a node that
// declares PXC alongside ps/psrepl/innodb cannot be installed at all — better to
// say so with both instance names than to fail halfway through a deploy with a
// dnf transaction error.

// aioIssues validates one All-in-One node.
//
// It is deliberately pure — no engine, no context. Every rule here is a property
// of the design document alone, which keeps them unit-testable and means a
// validation pass cannot fail because Docker hiccuped. The one fact it needs
// from outside, the host's memory, arrives as hostMemBytes (0 = unknown).
//
// exportReq is the stack-wide requested-host-port map, shared with every other
// node type so a collision between an AiO instance and a classic node is caught.
func aioIssues(n designNode, doc designDoc, exportReq map[int][]string, hostMemBytes int64) []issue {
	var out []issue
	label := strings.TrimSpace(n.Label)
	if label == "" {
		label = "All in One"
	}

	if len(n.AIOInstances) == 0 {
		out = append(out, issue{"warning", "All-in-One node " + label + " has no features added — it will deploy as a bare host"})
		return out
	}

	// --- the flavor conflict -------------------------------------------------
	if _, conflict := aioMySQLFlavor(n.AIOInstances); conflict {
		// Name every colliding flavor with the instances that pulled it in, so the
		// fix is obvious without counting kinds by hand.
		var parts []string
		for _, f := range aioMySQLFlavorsUsed(n.AIOInstances) {
			var names []string
			for _, in := range n.AIOInstances {
				if aioMySQLFlavorOfKind(in.Kind) == f {
					names = append(names, in.Name)
				}
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", aioFlavorLabel(f), strings.Join(names, ", ")))
		}
		out = append(out, issue{"error", fmt.Sprintf(
			"All-in-One node %s declares more than one MySQL flavor: %s. "+
				"Each of these server packages Provides: mysql-server and conflicts with the others, "+
				"so only one can be installed per container — keep one set and remove the rest.",
			label, strings.Join(parts, " and "))})
	}

	// --- the PostgreSQL flavor conflict --------------------------------------
	// Same shape as the MySQL one but scoped to a major, because PPG and PGDG
	// packages only collide within the same major (see aioPGFlavorConflicts).
	for major, flavors := range aioPGFlavorConflicts(n.AIOInstances) {
		// Stable ordering so the message does not shuffle between runs.
		var parts []string
		for _, f := range []string{pgFlavorPercona, pgFlavorPGDG, pgFlavorSource} {
			if names := flavors[f]; len(names) > 0 {
				parts = append(parts, fmt.Sprintf("%s (%s)", aioPGFlavorLabel[f], strings.Join(names, ", ")))
			}
		}
		out = append(out, issue{"error", fmt.Sprintf(
			"All-in-One node %s needs PostgreSQL %s from more than one source: %s. "+
				"They all install to /usr/pgsql-%s and cannot coexist — repmgr needs PGDG's postgresql%s-server "+
				"(which percona-postgresql%s-server does not provide) and Spock builds a patched PostgreSQL into the same "+
				"prefix. Put them on different PostgreSQL majors, or drop all but one.",
			label, major, strings.Join(parts, " and "), major, major, major)})
	}

	// --- per-instance rules --------------------------------------------------
	names := map[string]int{}
	instIDs := map[string]bool{}
	for _, in := range n.AIOInstances {
		instIDs[in.ID] = true
	}
	for _, in := range n.AIOInstances {
		k, ok := aioKindOf(in.Kind)
		if !ok {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One node %s has an instance of unknown kind %q", label, in.Kind)})
			continue
		}
		iname := strings.TrimSpace(in.Name)
		if iname == "" {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One node %s has an unnamed %s instance", label, k.Label)})
			continue
		}
		if aioSanitizeInst(iname) != strings.ToLower(iname) || aioSanitizeInst(iname) == "" {
			out = append(out, issue{"error", fmt.Sprintf(
				"All-in-One instance %q on node %s is not a valid name — use lowercase letters, digits and dashes (it becomes a hostname, a directory and a systemd unit)", iname, label)})
		}
		names[aioSanitizeInst(iname)]++

		// Kinds whose provisioner does not exist yet must not deploy: a node that
		// silently skipped half its instances would report "running".
		if !aioSupportedFamilies[k.Family] || aioUnsupportedKinds[in.Kind] {
			out = append(out, issue{"error", fmt.Sprintf(
				"All-in-One node %s declares a %s instance (%s), which is not implemented yet — remove it or use a dedicated %s node",
				label, k.Label, iname, k.Label)})
		}

		// Options the form must not promise: accepted into the design but not yet
		// honoured by any provisioner. Silently ignoring them is worse than the
		// feature being absent — a user ticks the box and gets nothing, with no
		// indication why. PMM is deliberately NOT in this list; it is implemented.
		for _, o := range aioUnimplementedOptions(in) {
			out = append(out, issue{"error", fmt.Sprintf(
				"All-in-One instance %s has %s enabled, which All-in-One does not implement yet — "+
					"turn it off, or use a dedicated node for that database", iname, o)})
		}

		if why, blocked := aioUnsupportedModes[in.Kind+":"+strings.TrimSpace(in.ReplMode)]; blocked {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s: %s", iname, why)})
		}

		if k.Cluster {
			m := in.Members
			if m < k.MinMem || m > k.MaxMem {
				out = append(out, issue{"error", fmt.Sprintf(
					"All-in-One instance %s (%s) has %d member(s) — allowed range is %d–%d", iname, k.Label, m, k.MinMem, k.MaxMem)})
			} else if k.OddOnly && m%2 == 0 {
				out = append(out, issue{"error", fmt.Sprintf(
					"All-in-One instance %s (%s) needs an odd member count for quorum — %d is even", iname, k.Label, m)})
			}
		}

		// Drop-down references must resolve. These replace association lines, so
		// a dangling one is the equivalent of an edge to a deleted node.
		if in.PMMNodeID != "" && !nodeOfTypeExists(doc, in.PMMNodeID, "pmm") {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s is monitored by a PMM node that is not on the canvas", iname)})
		}
		if in.OpenBaoNodeID != "" && !nodeOfTypeExists(doc, in.OpenBaoNodeID, "openbao") {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s references an OpenBao node that is not on the canvas", iname)})
		}
		if in.KeycloakNodeID != "" && !nodeOfTypeExists(doc, in.KeycloakNodeID, "keycloak") {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s references a Keycloak node that is not on the canvas", iname)})
		}
		if in.SeaweedFSNodeID != "" && !nodeOfTypeExists(doc, in.SeaweedFSNodeID, "seaweedfs") {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s references a SeaweedFS node that is not on the canvas", iname)})
		}
		if in.LdapAuth && in.LdapDirNodeID != "" &&
			!nodeOfTypeExists(doc, in.LdapDirNodeID, "intranet") && !nodeOfTypeExists(doc, in.LdapDirNodeID, "sambaad") {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s authenticates against a directory node that is not on the canvas", iname)})
		}
		// Orchestrator may be a canvas node OR another instance in this node.
		if ref := in.OrchestratorRef; ref != "" {
			if local, isLocal := strings.CutPrefix(ref, "inst:"); isLocal {
				if !instIDs[local] {
					out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s is monitored by an Orchestrator instance that no longer exists on this node", iname)})
				}
			} else if !nodeOfTypeExists(doc, ref, "orchestrator") {
				out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s is monitored by an Orchestrator node that is not on the canvas", iname)})
			}
		}
		// A proxy must front something, and that something must be a database.
		if k.Family == famProxy || k.Family == famHAProxy {
			if in.BackendInstance == "" {
				out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s (%s) has no backend selected — pick the instance it should front", iname, k.Label)})
			} else if !instIDs[in.BackendInstance] {
				out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s (%s) fronts an instance that no longer exists on this node", iname, k.Label)})
			} else if b := aioInstanceByID(n.AIOInstances, in.BackendInstance); b != nil && !aioProxyCanFront(in.Kind, b.Kind) {
				out = append(out, issue{"error", fmt.Sprintf("All-in-One instance %s (%s) cannot front a %s instance", iname, k.Label, aioKindByID[b.Kind].Label)})
			}
		}
		if in.ExportEnabled && in.ExportHostPort != 0 {
			exportReq[in.ExportHostPort] = append(exportReq[in.ExportHostPort], label+"/"+iname)
		}
	}
	for name, count := range names {
		if count > 1 {
			out = append(out, issue{"error", fmt.Sprintf("All-in-One node %s has %d instances named %q — names must be unique within the node", label, count, name)})
		}
	}

	// --- node-level sanity ---------------------------------------------------
	for _, fam := range aioFamiliesUsed(n.AIOInstances) {
		if used := aioSlotsUsed(n.AIOInstances, fam); used > aioSlotsPerFamily {
			out = append(out, issue{"error", fmt.Sprintf(
				"All-in-One node %s needs %d %s port slots but only %d are reserved per family", label, used, fam, aioSlotsPerFamily)})
		}
	}
	orch := 0
	daemons := 0
	for _, in := range n.AIOInstances {
		daemons += aioMemberCount(in.Kind, in.Members)
		if in.Kind == "orchestrator" {
			orch++
		}
	}
	if orch > 1 {
		out = append(out, issue{"warning", fmt.Sprintf("All-in-One node %s has %d Orchestrator instances — one discovers every cluster in the stack already", label, orch)})
	}
	if daemons > 20 {
		out = append(out, issue{"warning", fmt.Sprintf("All-in-One node %s runs %d database daemons in one container — expect a slow deploy and high memory use", label, daemons)})
	}
	// Compare the estimated footprint against the node's own cap, then the host.
	if est := aioEstMemMB(n.AIOInstances); est > 0 {
		if n.MemoryGB > 0 && est > n.MemoryGB*1024 {
			out = append(out, issue{"warning", fmt.Sprintf(
				"All-in-One node %s is capped at %d GiB but its instances need roughly %d GiB — raise the memory limit or remove instances", label, n.MemoryGB, (est+1023)/1024)})
		} else if hostMemBytes > 0 && int64(est)*1024*1024 > hostMemBytes {
			out = append(out, issue{"warning", fmt.Sprintf(
				"All-in-One node %s needs roughly %d GiB but the host has %d GiB", label, (est+1023)/1024, hostMemBytes/(1024*1024*1024))})
		}
	}
	return out
}

// nodeOfTypeExists reports whether the design contains a node with this id and type.
func nodeOfTypeExists(doc designDoc, id, typ string) bool {
	for _, n := range doc.Nodes {
		if n.ID == id && n.Type == typ {
			return true
		}
	}
	return false
}

// aioInstanceByID finds a declared instance by its id.
func aioInstanceByID(instances []aioInstance, id string) *aioInstance {
	for i := range instances {
		if instances[i].ID == id {
			return &instances[i]
		}
	}
	return nil
}

// aioProxyCanFront reports whether a proxy kind can sit in front of a backend
// kind: ProxySQL speaks the MySQL protocol, HAProxy load-balances either family
// at layer 4 (a PostgreSQL cluster or a MySQL one), and neither fronts a proxy.
func aioProxyCanFront(proxyKind, backendKind string) bool {
	backendFam := aioFamilyOf(backendKind)
	switch proxyKind {
	case "proxysql":
		return backendFam == famMySQL
	case "haproxy":
		return backendFam == famMySQL || backendFam == famPG
	}
	return false
}

// aioUnimplementedOptions lists the per-instance options a design has enabled
// that no provisioner reads yet.
//
// This exists because the opposite failure is silent: the instance form renders
// a control, validation accepts it, the deploy succeeds, and the user is left
// wondering why their database is not monitored / encrypted / backed up. Each
// entry here is a promise the UI must not make until the wiring exists, and each
// disappears from this list the moment its provisioner lands.
func aioUnimplementedOptions(in aioInstance) []string {
	var out []string
	if in.LdapAuth {
		out = append(out, "directory (LDAP) authentication")
	}
	if in.EnableVault {
		out = append(out, "data-at-rest encryption with OpenBao")
	}
	if in.SeaweedFSNodeID != "" {
		out = append(out, "a SeaweedFS backup target")
	}
	// TLS is implemented for the three database engines; Valkey and the proxies
	// have their own very different TLS shapes and are not wired yet.
	if in.GenerateCert && !aioTLSSupported(in.Kind) {
		out = append(out, "TLS certificates from the Intranet CA")
	}
	if in.EnableOIDC {
		out = append(out, "Keycloak OIDC authentication")
	}
	return out
}
