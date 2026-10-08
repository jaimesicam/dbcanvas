package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// dbusers.go — database accounts across a cluster: list them with which members have each one
// (and whether its password is the same everywhere), and create, grant, rotate or drop one on the
// primary, then confirm the change reached every member. Replication carries an account to the
// other members; "carries" is what a DBA checks, because a replica that is not applying is one
// where the new application account does not exist yet and the rotated password still works.
//
// MySQL, Percona Server, MariaDB, PXC/Galera and Group Replication through the mysql client;
// PostgreSQL (Patroni, repmgr) through psql; MongoDB replica sets through the driver.

var (
	dbUserNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)
	dbHostRe     = regexp.MustCompile(`^[A-Za-z0-9_.%:-]{1,60}$`)
	dbNameRe     = regexp.MustCompile(`^[A-Za-z0-9_]{0,64}$`)
)

// dbSystemUsers are the engines' own accounts, never listed.
var dbSystemUsers = map[string]bool{
	"mysql.sys": true, "mysql.session": true, "mysql.infoschema": true, "mariadb.sys": true,
	"mysql_innodb_cluster_metadata": true,
}

type dbUser struct {
	Name     string   `json:"name"`
	Host     string   `json:"host,omitempty"`
	DB       string   `json:"db,omitempty"`    // MongoDB: the user's database
	Roles    string   `json:"roles,omitempty"` // a short summary of what it may do
	On       []string `json:"on"`              // member node ids that have it
	Differs  []string `json:"differs,omitempty"`
	Internal bool     `json:"internal,omitempty"`
}

type dbUsersReq struct {
	Action    string `json:"action"` // create | password | drop
	Name      string `json:"name"`
	Host      string `json:"host"`      // MySQL; "%" by default
	Password  string `json:"password"`  // create, password
	Privilege string `json:"privilege"` // readonly | readwrite | admin
	Database  string `json:"database"`  // "" = every database
}

// memberAccounts is one member's accounts: "name@host" (or "db.name") -> a hash of its
// credential, so members can be compared without the hash leaving the server unhashed.
type memberAccounts map[string]struct{ cred, roles string }

func (a *App) usersTopology(ctx context.Context, st Stack, nid string) (swTopo, swMember, error) {
	t, err := a.groupTopology(st, nid, rebuildKinds, map[string]string{}, false)
	if err != nil {
		// A standalone server is its own cluster of one.
		return swTopo{}, swMember{}, err
	}
	states, pi := a.probeTopo(ctx, st, t)
	if t.Kind == "galera" {
		for _, s := range states {
			if s.State == "Synced" {
				m, _ := t.member(s.NodeID)
				return t, m, nil
			}
		}
		return t, swMember{}, errors.New("no member is Synced")
	}
	if pi < 0 {
		return t, swMember{}, errors.New("no member reports itself primary right now")
	}
	m, _ := t.member(states[pi].NodeID)
	return t, m, nil
}

func credHash(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// accountsOn lists one member's accounts.
func (a *App) accountsOn(ctx context.Context, st Stack, m swMember) (memberAccounts, error) {
	c, ok := a.dbConnFor(st, m.Node.ID)
	if !ok {
		return nil, errors.New("not running")
	}
	out := memberAccounts{}
	switch c.Engine {
	case "mysql":
		var rows []struct {
			U, H, A, P string
		}
		if err := a.queryJSON(ctx, c, "", `SELECT JSON_ARRAYAGG(JSON_OBJECT('U', u.user, 'H', u.host, 'A', u.authentication_string,
		  'P', CONCAT_WS(',', IF(u.Super_priv='Y','SUPER',NULL), IF(u.Select_priv='Y','read all',NULL), IF(u.Insert_priv='Y','write all',NULL),
		    (SELECT GROUP_CONCAT(CONCAT(d.Db, IF(d.Insert_priv='Y',' rw',' r')) ORDER BY d.Db SEPARATOR ',') FROM mysql.db d WHERE d.User = u.user AND d.Host = u.host))))
		  FROM mysql.user u`, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.U+"@"+r.H] = struct{ cred, roles string }{credHash(r.A), r.P}
		}
	case "postgres":
		var rows []struct {
			N, P   string
			S, L   bool
			Member string
		}
		if err := a.queryJSON(ctx, c, "postgres", `SELECT coalesce(json_agg(json_build_object('N', r.rolname, 'P', coalesce(r.rolpassword,''), 'S', r.rolsuper, 'L', r.rolcanlogin,
		  'Member', (SELECT string_agg(g.rolname, ',' ORDER BY g.rolname) FROM pg_auth_members am JOIN pg_roles g ON g.oid = am.roleid WHERE am.member = r.oid))), '[]') FROM pg_authid r WHERE r.rolname !~ '^pg_'`, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			if !r.L {
				continue
			}
			roles := r.Member
			if r.S {
				roles = strings.Trim("superuser,"+roles, ",")
			}
			out[r.N] = struct{ cred, roles string }{credHash(r.P), roles}
		}
	case "mongodb":
		client, closer, err := a.mongoClientFor(ctx, c)
		if err != nil {
			return nil, err
		}
		defer closer()
		var res struct {
			Users []struct {
				User  string `bson:"user"`
				DB    string `bson:"db"`
				Roles []struct {
					Role string `bson:"role"`
					DB   string `bson:"db"`
				} `bson:"roles"`
				Credentials bson.M `bson:"credentials"`
			} `bson:"users"`
		}
		cmd := bson.D{{Key: "usersInfo", Value: bson.D{{Key: "forAllDBs", Value: true}}}, {Key: "showCredentials", Value: true}}
		if err := client.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
			return nil, err
		}
		for _, u := range res.Users {
			var roles []string
			for _, r := range u.Roles {
				roles = append(roles, r.Role+"@"+r.DB)
			}
			b, _ := json.Marshal(u.Credentials)
			out[u.DB+"."+u.User] = struct{ cred, roles string }{credHash(string(b)), strings.Join(roles, ",")}
		}
	default:
		return nil, errors.New("accounts are not managed here for this engine")
	}
	return out, nil
}

// clusterAccounts asks every member at once.
func (a *App) clusterAccounts(ctx context.Context, st Stack, t swTopo) ([]memberAccounts, []string) {
	accs := make([]memberAccounts, len(t.Members))
	errs := make([]string, len(t.Members))
	var wg sync.WaitGroup
	for i, m := range t.Members {
		wg.Add(1)
		go func(i int, m swMember) {
			defer wg.Done()
			c := withEngine(ctx, a.depEngine(st, m.Node.ID))
			acc, err := a.accountsOn(c, st, m)
			if err != nil {
				errs[i] = clipLine(err.Error(), 160)
				return
			}
			accs[i] = acc
		}(i, m)
	}
	wg.Wait()
	return accs, errs
}

// handleDBUsers is GET /api/stacks/{id}/nodes/{nid}/db-users.
func (a *App) handleDBUsers(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	t, primary, err := a.usersTopology(ctx, st, r.PathValue("nid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	accs, errs := a.clusterAccounts(ctx, st, t)
	writeJSON(w, http.StatusOK, map[string]any{"kind": t.Kind, "primary": primary.Node.ID, "members": membersOut(t, errs), "users": mergeAccounts(t, primary, accs)})
}

func membersOut(t swTopo, errs []string) []map[string]string {
	out := []map[string]string{}
	for i, m := range t.Members {
		out = append(out, map[string]string{"nodeId": m.Node.ID, "label": m.Node.Label, "error": errs[i]})
	}
	return out
}

// mergeAccounts lists every account any member has, with which members have it and which of
// those hold a different credential from the primary's.
func mergeAccounts(t swTopo, primary swMember, accs []memberAccounts) []dbUser {
	pi := -1
	for i, m := range t.Members {
		if m.Node.ID == primary.Node.ID {
			pi = i
		}
	}
	keys := map[string]bool{}
	for _, acc := range accs {
		for k := range acc {
			keys[k] = true
		}
	}
	out := []dbUser{}
	for k := range keys {
		u := dbUser{On: []string{}}
		switch {
		case t.Kind == "mongo":
			u.DB, u.Name, _ = strings.Cut(k, ".")
		case strings.Contains(k, "@"):
			i := strings.LastIndex(k, "@")
			u.Name, u.Host = k[:i], k[i+1:]
		default:
			u.Name = k
		}
		if dbSystemUsers[u.Name] {
			continue
		}
		var want string
		if pi >= 0 && accs[pi] != nil {
			want = accs[pi][k].cred
			u.Roles = accs[pi][k].roles
		}
		// Galera replicates the statement, password and all, and every member hashes it with
		// a salt of its own: there the hashes differ by design, and only presence is compared.
		if t.Kind == "galera" {
			want = ""
		}
		for i, acc := range accs {
			if acc == nil {
				continue
			}
			v, ok := acc[k]
			if !ok {
				continue
			}
			u.On = append(u.On, t.Members[i].Node.ID)
			if u.Roles == "" {
				u.Roles = v.roles
			}
			if want != "" && v.cred != want {
				u.Differs = append(u.Differs, t.Members[i].Node.ID)
			}
		}
		u.Internal = strings.HasPrefix(u.Name, "mysql_innodb_") || u.Name == "repl" || u.Name == "replicator" || u.Name == "pmm" ||
			u.Name == "monitor" || u.Name == "postgres" || u.Name == "root" || strings.HasPrefix(u.Name, "pbm") || u.Name == "repmgr"
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Internal != out[j].Internal {
			return !out[i].Internal
		}
		return out[i].DB+out[i].Name+out[i].Host < out[j].DB+out[j].Name+out[j].Host
	})
	return out
}

// handleDBUsersChange is POST /api/stacks/{id}/nodes/{nid}/db-users.
func (a *App) handleDBUsersChange(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var in dbUsersReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Host == "" {
		in.Host = "%"
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	t, primary, err := a.usersTopology(ctx, st, r.PathValue("nid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err := a.applyUserChange(ctx, st, t, primary, in); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// Confirm: every member has (or no longer has) the account, with the primary's credential.
	key := in.Name
	switch t.Kind {
	case "mongo":
		key = firstNonEmpty(in.Database, "admin") + "." + in.Name
	case "patroni", "repmgr":
	default:
		key = in.Name + "@" + in.Host
	}
	var confirmed, missing []string
	deadline := time.Now().Add(15 * time.Second)
	for {
		accs, _ := a.clusterAccounts(ctx, st, t)
		confirmed, missing = nil, nil
		var want string
		for i, m := range t.Members {
			if m.Node.ID == primary.Node.ID && accs[i] != nil {
				want = accs[i][key].cred
			}
		}
		for i, m := range t.Members {
			v, has := accs[i][key]
			same := v.cred == want || t.Kind == "galera" // see mergeAccounts
			ok := accs[i] != nil && (in.Action == "drop" && !has || in.Action != "drop" && has && same)
			if ok {
				confirmed = append(confirmed, m.Node.Label)
			} else {
				missing = append(missing, m.Node.Label)
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	what := map[string]string{"create": "created", "password": "password changed for", "drop": "dropped"}[in.Action]
	a.recordStackEvent(st.ID, primary.Node.ID, "action", map[bool]string{true: "info", false: "warning"}[len(missing) == 0],
		fmt.Sprintf("Database user %s %s on %s", what, in.Name, t.Frame.Label),
		map[bool]string{true: "confirmed on every member", false: "not yet on " + strings.Join(missing, ", ")}[len(missing) == 0], u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "on": primary.Node.Label, "confirmed": confirmed, "missing": missing})
}

func (in dbUsersReq) validate() error {
	if !dbUserNameRe.MatchString(in.Name) {
		return errors.New("a user name is a letter, then up to 31 letters, digits, _ . or -")
	}
	if !dbHostRe.MatchString(in.Host) {
		return errors.New("the host is a name, an address or a pattern with %")
	}
	if !dbNameRe.MatchString(in.Database) {
		return errors.New("a database name is letters, digits and _")
	}
	if dbSystemUsers[in.Name] {
		return errors.New("that is one of the server's own accounts")
	}
	switch in.Action {
	case "create", "password":
		if len(in.Password) < 8 || strings.ContainsAny(in.Password, `'"\`+"`$") {
			return errors.New("the password needs at least 8 characters, and none of ' \" \\ ` $")
		}
	case "drop":
	default:
		return errors.New("action is create, password or drop")
	}
	if in.Action == "create" {
		switch in.Privilege {
		case "readonly", "readwrite", "admin":
		default:
			return errors.New("privilege is readonly, readwrite or admin")
		}
	}
	return nil
}

// applyUserChange makes the change on the primary; replication takes it to the others.
func (a *App) applyUserChange(ctx context.Context, st Stack, t swTopo, primary swMember, in dbUsersReq) error {
	c, ok := a.dbConnFor(st, primary.Node.ID)
	if !ok {
		return errors.New("the primary is not running")
	}
	c2 := withEngine(ctx, a.depEngine(st, primary.Node.ID))
	switch c.Engine {
	case "mysql":
		acct := fmt.Sprintf("'%s'@'%s'", in.Name, in.Host)
		on := "*.*"
		if in.Database != "" {
			on = "`" + in.Database + "`.*"
		}
		var sql string
		switch in.Action {
		case "create":
			priv := map[string]string{
				"readonly":  "SELECT, SHOW VIEW",
				"readwrite": "SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, INDEX, ALTER, CREATE TEMPORARY TABLES, LOCK TABLES, EXECUTE, CREATE VIEW, SHOW VIEW",
				"admin":     "ALL PRIVILEGES",
			}[in.Privilege]
			grant := ""
			if in.Privilege == "admin" {
				grant = " WITH GRANT OPTION"
			}
			sql = fmt.Sprintf("CREATE USER %s IDENTIFIED BY '%s'; GRANT %s ON %s TO %s%s;", acct, in.Password, priv, on, acct, grant)
		case "password":
			sql = fmt.Sprintf("ALTER USER %s IDENTIFIED BY '%s';", acct, in.Password)
		case "drop":
			sql = fmt.Sprintf("DROP USER %s;", acct)
		}
		return a.execSQL(c2, c, "mysql", sql)
	case "postgres":
		role := `"` + in.Name + `"`
		var sql string
		switch in.Action {
		case "create":
			sql = fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s';", role, in.Password)
			switch in.Privilege {
			case "readonly":
				sql += fmt.Sprintf(" GRANT pg_read_all_data TO %s;", role)
			case "readwrite":
				sql += fmt.Sprintf(" GRANT pg_read_all_data, pg_write_all_data TO %s;", role)
			case "admin":
				sql += fmt.Sprintf(" ALTER ROLE %s SUPERUSER;", role)
			}
		case "password":
			sql = fmt.Sprintf("ALTER ROLE %s PASSWORD '%s';", role, in.Password)
		case "drop":
			sql = fmt.Sprintf("DROP ROLE %s;", role)
		}
		return a.execSQL(c2, c, "postgres", sql)
	case "mongodb":
		client, closer, err := a.mongoClientFor(c2, c)
		if err != nil {
			return err
		}
		defer closer()
		db := firstNonEmpty(in.Database, "admin")
		var cmd bson.D
		switch in.Action {
		case "create":
			var roles bson.A
			if in.Database == "" {
				role := map[string]string{"readonly": "readAnyDatabase", "readwrite": "readWriteAnyDatabase", "admin": "root"}[in.Privilege]
				roles = bson.A{bson.D{{Key: "role", Value: role}, {Key: "db", Value: "admin"}}}
			} else {
				role := map[string]string{"readonly": "read", "readwrite": "readWrite", "admin": "dbOwner"}[in.Privilege]
				roles = bson.A{bson.D{{Key: "role", Value: role}, {Key: "db", Value: in.Database}}}
			}
			cmd = bson.D{{Key: "createUser", Value: in.Name}, {Key: "pwd", Value: in.Password}, {Key: "roles", Value: roles},
				{Key: "writeConcern", Value: bson.D{{Key: "w", Value: "majority"}}}}
		case "password":
			cmd = bson.D{{Key: "updateUser", Value: in.Name}, {Key: "pwd", Value: in.Password}}
		case "drop":
			cmd = bson.D{{Key: "dropUser", Value: in.Name}}
		}
		return client.Database(db).RunCommand(c2, cmd).Err()
	}
	return errors.New("accounts are not managed here for this engine")
}
