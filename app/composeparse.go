package main

import (
	"fmt"
	"strconv"
	"strings"
)

// composeparse.go — the short form of a compose entry, "kind[:count][,key=value|,flag]...",
// as a stack file's `nodes:` list and the command line write it. It maps every option to
// one field of composeNodeSpec, and refuses an unknown one with the valid set rather than
// letting it become a field nobody reads.

// parseComposeNode turns "pxc:3,version=8.4.5,os=el8,monitor" into a spec entry.
func parseComposeNode(arg string) (map[string]any, error) {
	parts := strings.Split(arg, ",")
	head := strings.TrimSpace(parts[0])
	if head == "" {
		return nil, fmt.Errorf("empty node spec")
	}
	out := map[string]any{}

	// kind[:count]
	if i := strings.Index(head, ":"); i > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(head[i+1:]))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q: the count after ':' must be a positive number", head)
		}
		out["kind"] = strings.TrimSpace(head[:i])
		out["count"] = n
	} else {
		out["kind"] = head
	}

	// Options: bare words are booleans, key=value is a value. Listed explicitly
	// rather than passed through, so a typo is caught here with the valid set
	// rather than becoming a field the server silently ignores.
	boolOpts := map[string]string{
		// The relationships: each wires this node to another in the spec.
		"monitor": "monitor", "ldap": "ldap", "oidc": "oidc",
		"kerberos": "kerberos", "vault": "vault", "backup": "backup",
		"orchestrator": "orchestrator",
		// And the plain switches.
		"export": "export", "cert": "cert", "proxy": "proxy", "gtid": "gtid",
		"mysqlrouter": "mysqlRouter", "tls": "tls",
		"netalltraffic": "netAllTraffic", "pgvector": "pgvector",
		"pgaudit": "pgaudit", "setuser": "setUser", "tdewal": "tdeWal",
		"mca": "mca", "viewonly": "viewOnly",
	}
	strOpts := map[string]string{
		"name": "name", "os": "os", "arch": "arch", "version": "version", "runtime": "runtime", "selinux": "selinux",
		// "…With" names which provider, when a spec has more than one.
		"monitorwith": "monitorWith", "ldapwith": "ldapWith", "oidcwith": "oidcWith",
		"kerberoswith": "kerberosWith", "vaultwith": "vaultWith",
		"backupwith": "backupWith", "orchestratorwith": "orchestratorWith",
		// "to" is the association line — the backend a proxy fronts, the database a
		// simulator drives. Omit it and the server picks the only legal target, or
		// refuses and names the choices.
		"to": "to",
		// "standbyOf" makes a Patroni cluster a standby cluster of another one in the spec.
		"standbyof": "standbyOf",
		// Per-engine.
		"replmode": "replMode", "mode": "mode", "setup": "setup",
		"certttl": "certTtl", "pgauditlog": "pgauditLog",
		"alertemail": "alertEmail",
		// haproxy: os, percona or pgdg — where the package comes from.
		"source": "source",
	}
	intOpts := map[string]string{
		"count": "count", "cpus": "cpus", "memorygb": "memoryGb", "exportport": "exportPort",
		"pgbench": "pgbench", "replicas": "replicas", "syncstandbys": "syncStandbys",
		// Resource shaping.
		"netlatencyms": "netLatencyMs", "netjitterms": "netJitterMs",
		"netratembit":    "netRateMbit",
		"devicereadmbps": "deviceReadMbps", "devicewritembps": "deviceWriteMbps",
	}
	// Two that are neither a string nor a whole number.
	floatOpts := map[string]string{"netlosspct": "netLossPct"}
	listOpts := map[string]string{"buckets": "buckets"}

	for _, raw := range parts[1:] {
		opt := strings.TrimSpace(raw)
		if opt == "" {
			continue
		}
		// opt.FIELD=VALUE and member.FIELD=VALUE set any catalogued field by its design
		// name, as a stack file's options and memberOptions do: the frame's (or the
		// node's) own, and every member's.
		if k, v, ok := strings.Cut(opt, "="); ok {
			for _, pfx := range []string{"opt.", "member."} {
				if field, isOpt := strings.CutPrefix(strings.TrimSpace(k), pfx); isOpt && field != "" {
					key := "options"
					if pfx == "member." {
						key = "memberOptions"
					}
					m, _ := out[key].(map[string]any)
					if m == nil {
						m = map[string]any{}
						out[key] = m
					}
					val, err := composeOptionValue(fmt.Sprint(out["kind"]), pfx == "member.", field, strings.TrimSpace(v))
					if err != nil {
						return nil, err
					}
					m[field] = val
					opt = ""
				}
			}
			if opt == "" {
				continue
			}
		}
		key, val, hasVal := strings.Cut(opt, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		if field, ok := boolOpts[key]; ok {
			b := true
			if hasVal {
				var err error
				if b, err = strconv.ParseBool(val); err != nil {
					return nil, fmt.Errorf("%s=%s: want true or false", key, val)
				}
			}
			out[field] = b
			continue
		}
		if field, ok := strOpts[key]; ok {
			if !hasVal {
				return nil, fmt.Errorf("%s needs a value, as %s=…", key, key)
			}
			out[field] = val
			continue
		}
		if field, ok := intOpts[key]; ok {
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("%s=%s: want a number", key, val)
			}
			out[field] = n
			continue
		}
		if field, ok := floatOpts[key]; ok {
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return nil, fmt.Errorf("%s=%s: want a number", key, val)
			}
			out[field] = f
			continue
		}
		if field, ok := listOpts[key]; ok {
			// "buckets=backups+wal" rather than a comma: commas already separate
			// options, and quoting one inside a --node value is a trap.
			out[field] = strings.Split(val, "+")
			continue
		}
		return nil, fmt.Errorf("unknown option %q in node spec %q.\n"+
			"  links:   monitor, ldap, oidc, kerberos, vault, backup, orchestrator\n"+
			"           (each takes an optional <link>With= naming which node)\n"+
			"  wire to: to=<name>  — the backend a proxy fronts, the database a sim drives\n"+
			"           standbyOf=<name> — a Patroni cluster as a standby of another\n"+
			"  flags:   export, cert, proxy, gtid, mysqlRouter, tls, netAllTraffic, pgvector,\n"+
			"           pgaudit, setUser, tdeWal, mca, viewOnly\n"+
			"  any:     opt.<field>=, member.<field>= — a catalogued field by its design name\n"+
			"  values:  name=, os=, arch=, version=, count=, cpus=, memoryGb=, exportPort=,\n"+
			"           replMode=, mode=, setup=, certTtl=, pgauditLog=, pgbench=, replicas=,\n"+
			"           syncStandbys=, alertEmail=, source=, buckets=a+b\n"+
			"  shaping: netLatencyMs=, netJitterMs=, netLossPct=, netRateMbit=,\n"+
			"           deviceReadMbps=, deviceWriteMbps=\n"+
			"  `dbcanvas kinds` lists what each kind supports.", key, arg)
	}
	return out, nil
}

// composeOptionValue types an opt. or member. value as the design catalogue types the
// field, so opt.k3dNodes=2 is a number and opt.k3dOperatorVer=1.20.0 stays a string. A
// field the catalogue does not know is left a string; applying it reports the error.
func composeOptionValue(kind string, member bool, field, val string) (any, error) {
	typ := "string"
	if k, ok := composeKindByName(kind); ok {
		var e *catalogEntry
		var found bool
		if k.Frame && !member {
			e, found = designCatalog().frame(k.Type)
		} else if k.Frame {
			if fe, ok := designCatalog().frame(k.Type); ok {
				e, found = designCatalog().node(fe.MemberType)
			}
		} else {
			e, found = designCatalog().node(k.Type)
		}
		if found {
			if o, ok := e.option(field); ok {
				typ = o.Type
			}
		}
	}
	switch typ {
	case "boolean":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return nil, fmt.Errorf("%s=%s: want true or false", field, val)
		}
		return b, nil
	case "integer":
		n, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("%s=%s: want a whole number", field, val)
		}
		return n, nil
	case "number":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, fmt.Errorf("%s=%s: want a number", field, val)
		}
		return f, nil
	}
	return val, nil
}
