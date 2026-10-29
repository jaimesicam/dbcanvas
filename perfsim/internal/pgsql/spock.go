package pgsql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"perfsim/internal/core"
)

// A Spock cluster is multi-master PostgreSQL with logical replication: every
// member takes writes and subscribes to every other. It replicates one
// database, and only the tables in a replication set; it does not replicate
// DDL here. So the sim works in a perfsim schema of that database, creates its
// tables on every member, and adds the ones whose rows should reach the others
// (the heartbeat, orders, prepared datasets) to the default replication set.
// The first member is the writer: the heartbeat is written there, and every
// other member's copy of it is how far that member's apply is behind.

// schema is the sim's schema inside a Spock cluster's database.
const schema = "perfsim"

func (e *Engine) spock() bool { return e.cfg.Manager == "spock" }

// schema is where the sim's tables are: public in its own database, perfsim in
// a database it shares (see Config.Database).
func (e *Engine) schema() string {
	if e.cfg.Database != "" {
		return schema
	}
	return "public"
}

// spockInit creates the schema on every member and points the heartbeat at the
// writer member itself, so lag is measured from one origin even when the
// application writes through an HAProxy that may pick another member.
func (e *Engine) spockInit(ctx context.Context, writer string) error {
	for id, d := range e.mdb {
		if _, err := d.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			return fmt.Errorf("creating the %s schema on %s: %w", schema, id, err)
		}
	}
	for _, m := range e.cfg.Members {
		if m.ID == writer {
			old := e.hb.Swap(e.open(m.Host, m.Port, 2))
			old.Close()
		}
	}
	return nil
}

// spockReplicate adds a table to the default replication set on every member.
func (e *Engine) spockReplicate(ctx context.Context, table string) error {
	for id, d := range e.mdb {
		_, err := d.ExecContext(ctx, "SELECT spock.repset_add_table('default', $1::regclass)", schema+"."+table)
		if err != nil && !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("adding %s to Spock's replication set on %s: %w", table, id, err)
		}
	}
	return nil
}

// spockSample fills a peer's member entry: its subscriptions' status, and its
// lag behind the writer from its copy of the heartbeat (already read).
func (e *Engine) spockSample(ctx context.Context, d *sql.DB, m *core.Member, writer bool, writerID string) {
	type sub struct{ name, status, provider string }
	var subs []sub
	rows, err := d.QueryContext(ctx, "SELECT subscription_name, status, provider_node FROM spock.sub_show_status()")
	if err == nil {
		for rows.Next() {
			var s sub
			if rows.Scan(&s.name, &s.status, &s.provider) == nil {
				subs = append(subs, s)
			}
		}
		rows.Close()
	}
	down := []string{}
	var list []string
	for _, s := range subs {
		list = append(list, s.provider+" "+s.status)
		if s.status != "replicating" {
			down = append(down, s.provider)
		}
	}
	m.Fields = append(m.Fields, [2]string{"subscriptions (provider status)", short(nz(strings.Join(list, ", "), "none"), 120)})
	if writer {
		m.Role, m.Lag = "writer", 0
		m.Chip, m.ChipText = "ok", "writer"
		return
	}
	m.Role, m.Source = "peer", writerID
	switch {
	case len(down) > 0:
		m.Chip, m.ChipText = "bad", "subscription "+strings.Join(down, ", ")+" not replicating"
	case m.Lag > 30:
		m.Chip, m.ChipText = "bad", "lagging"
	case m.Lag > 5:
		m.Chip, m.ChipText = "warn", "behind"
	default:
		m.Chip, m.ChipText = "ok", "in sync"
	}
}

// runSpockPause disables each target's subscriptions: nothing from the other
// members is applied there, and its lag grows second for second.
func (e *Engine) runSpockPause(ctx context.Context, j *core.Job, targets []string) {
	e.runEach(ctx, j, targets, "Subscriptions disabled", func(ctx context.Context, d *sql.DB, m string) (func(context.Context) error, error) {
		key := "undo:subenable:" + m
		e.saveUndo(ctx, key, undo{Kind: "subenable", Member: m})
		release := func(ctx context.Context) error {
			err := e.restore(ctx, undo{Kind: "subenable", Member: m})
			if err == nil {
				e.Delete(ctx, key)
			}
			return wrap(m, err)
		}
		_, err := d.ExecContext(ctx, "SELECT spock.sub_disable(sub_name, true) FROM spock.subscription")
		return release, err
	})
}

// spockEnable re-enables a member's subscriptions.
func spockEnable(ctx context.Context, d *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := d.ExecContext(ctx, "SELECT spock.sub_enable(sub_name, true) FROM spock.subscription")
	return err
}

// spockDials adapts the lag stressors to Spock: logical replication has no WAL
// replay to pause, delay or conflict with, so pausing becomes disabling the
// chosen peers' subscriptions, and the rest say why they do not apply.
func (e *Engine) spockDials(ds []core.Dial) []core.Dial {
	if e.cfg.Manager == "operator" {
		return operatorDials(ds)
	}
	if !e.spock() {
		return ds
	}
	why := map[string]string{
		"rewrite":  "Logical replication sends row changes, not pages: a VACUUM FULL on one member is not replayed on the others.",
		"conflict": "Spock members are primaries, not hot standbys: there is no replay for a query to conflict with.",
		"delay":    "recovery_min_apply_delay delays WAL replay on a standby; Spock members apply logical changes and have no such delay.",
	}
	for i := range ds {
		d := &ds[i]
		if d.Group != groupLag {
			continue
		}
		if w, ok := why[d.ID]; ok {
			d.Available, d.Why = false, w
		}
		switch d.ID {
		case "pause":
			d.Name = "Disable subscriptions"
			d.Desc = "Disables every subscription on the chosen members. Changes from the other members queue up in their replication slots, nothing is applied, and lag grows second for second until the subscriptions are enabled again."
			d.Warn = ""
			d.Watch = []string{"Replication lag", "Replication status"}
			d.SQL = "SELECT spock.sub_disable(sub_name, true) FROM spock.subscription;\n-- up to {hold} s later:\nSELECT spock.sub_enable(sub_name, true) FROM spock.subscription;"
		case "bigtxn":
			d.Desc = "One large UPDATE on a big table at a fixed interval, on the writer. Every changed row is decoded and sent to each other member, which applies it as one transaction before anything committed after it."
			d.SQL = "UPDATE perfsim.{table} SET note = note + 1\n WHERE id <= {rows};  -- every {every} s, one transaction"
		}
	}
	return ds
}

// operatorDials marks the stressors a Kubernetes operator would undo: its
// Patroni (or CloudNativePG's controller) resumes paused replay and owns the
// server's settings, and DBCanvas does not reach into the operator to stop it.
func operatorDials(ds []core.Dial) []core.Dial {
	why := map[string]string{
		"pause": "The Kubernetes operator's Patroni or controller resumes paused replay on its next loop, so a pause would not hold.",
		"delay": "The Kubernetes operator owns this cluster's PostgreSQL settings and would put recovery_min_apply_delay back.",
	}
	for i := range ds {
		if w, ok := why[ds[i].ID]; ok && ds[i].Group == groupLag {
			ds[i].Available, ds[i].Why = false, w
		}
	}
	return ds
}
