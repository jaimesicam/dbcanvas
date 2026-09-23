package main

// samplecode_postgres.go — the PostgreSQL samples.
//
// One thing shapes every template in this file: **CREATE DATABASE is not something a connection
// string can do for you**. A PostgreSQL client connects to a database that must already exist, and
// the statement that creates one cannot run inside a transaction. So every scenario that needs a
// schema opens a connection to the maintenance database first, creates the demo database if it is
// missing, and only then connects to it — which is exactly what a real application's bootstrap
// does, and exactly what the Ledger Sim node had to learn the hard way against a Patroni cluster.
//
// The account is the superuser, because a DBCanvas PostgreSQL node provisions no other role. The
// generated code says so in a comment rather than quietly modelling something no application
// should copy.

var scPostgresClients = []scClient{
	{
		Database: scPostgres, Language: "python", ID: "psycopg", Label: "psycopg 3",
		Summary: "The current PostgreSQL driver for Python. Server-side binding, context-managed connections, and libpq's own TLS vocabulary.",
		Runtime: scRuntimePython,
		Deps: []scDep{{
			Manager: "pip", Name: "psycopg[binary]", Import: "psycopg",
			License: "LGPL-3.0-only", URL: "https://www.psycopg.org/psycopg3/",
			Note: "LGPL-3.0 is compatible with the GPLv3 this generated code carries. The [binary] extra pulls a prebuilt libpq so nothing has to be compiled on the node.",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.py", "python", scPsycopgPy, g),
				scFileOf("requirements.txt", "text", "psycopg[binary]\n", g),
			}
		},
		Run: func(g scGen) string { return scVenv + "/bin/python crud.py" },
	},
	{
		Database: scPostgres, Language: "node", ID: "pg", Label: "pg (node-postgres)",
		Summary: "The standard Node.js PostgreSQL client. The sample uses a single Client rather than a Pool, so the connection lifecycle is visible.",
		Runtime: scRuntimeNode,
		Deps: []scDep{{
			Manager: "npm", Name: "pg", Version: "^8", License: "MIT",
			URL: "https://github.com/brianc/node-postgres",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.js", "javascript", scPgJS, g),
				scFileOf("package.json", "json", scNodePackageJSON, g),
			}
		},
		Run: func(g scGen) string { return "node crud.js" },
	},
	{
		Database: scPostgres, Language: "go", ID: "database-sql", Label: "database/sql + pgx",
		Summary: "pgx registered behind Go's database/sql interface — the portable half of pgx, and the one that looks like every other Go database program.",
		Runtime: scRuntimeGo,
		Deps: []scDep{{
			Manager: "gomod", Name: "github.com/jackc/pgx/v5", Version: "v5.10.0",
			License: "MIT", URL: "https://github.com/jackc/pgx",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("main.go", "go", scPgGo, g),
				scFileOf("go.mod", "text", scGoMod, g),
			}
		},
		Run: func(g scGen) string { return "go run ." },
	},
	{
		Database: scPostgres, Language: "java", ID: "jdbc", Label: "JDBC (pgJDBC)",
		Summary: "Plain JDBC over the PostgreSQL driver. Unlike Connector/J, pgJDBC reads a PEM certificate authority directly — no keystore to build.",
		Runtime: scRuntimeJava,
		Deps: []scDep{{
			Manager: "maven", Name: "org.postgresql:postgresql", Version: "42.7.7",
			License: "BSD-2-Clause", URL: "https://jdbc.postgresql.org/",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("src/main/java/DbCanvasCrud.java", "java", scPgJDBCJava, g),
				scFileOf("pom.xml", "xml", scMavenPOM, g),
			}
		},
		Run: func(g scGen) string { return scMavenRun },
	},
	{
		Database: scPostgres, Language: "java", ID: "hikari", Label: "JDBC + HikariCP",
		Summary: "pgJDBC behind a HikariCP pool: borrow a connection, use it, give it back, and close the pool at the end.",
		Runtime: scRuntimeJava,
		Deps: []scDep{
			{Manager: "maven", Name: "org.postgresql:postgresql", Version: "42.7.7", License: "BSD-2-Clause", URL: "https://jdbc.postgresql.org/"},
			{Manager: "maven", Name: "com.zaxxer:HikariCP", Version: "6.3.0", License: "Apache-2.0", URL: "https://github.com/brettwooldridge/HikariCP"},
			{
				Manager: "maven", Name: "org.slf4j:slf4j-api", Version: "2.0.19", License: "MIT",
				URL:  "https://www.slf4j.org/",
				Note: "Declared explicitly, not left to HikariCP: HikariCP 6.x still brings slf4j-api 1.7.x transitively, and a 1.7 API with a 2.0 binding finds no provider and logs nothing but a warning about it.",
			},
			{
				Manager: "maven", Name: "org.slf4j:slf4j-simple", Version: "2.0.19", License: "MIT",
				URL:  "https://www.slf4j.org/",
				Note: "HikariCP logs through SLF4J and needs a binding. slf4j-simple is MIT; Logback is EPL-1.0/LGPL-2.1 dual-licensed and EPL-1.0 is GPL-incompatible.",
			},
		},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("src/main/java/DbCanvasCrud.java", "java", scPgHikariJava, g),
				scFileOf("pom.xml", "xml", scMavenPOM, g),
			}
		},
		Run: func(g scGen) string { return scMavenRun },
	},
	{
		Database: scPostgres, Language: "dotnet", ID: "npgsql", Label: "Npgsql",
		Summary: "The .NET data provider for PostgreSQL, and the one Entity Framework Core's PostgreSQL support sits on. It speaks libpq's TLS vocabulary, down to VerifyFull and a root certificate path.",
		Runtime: scRuntimeDotnet,
		Deps: []scDep{{
			Manager: "nuget", Name: "Npgsql", Version: "10.0.3",
			License: "PostgreSQL", URL: "https://www.npgsql.org/",
		}},
		Files: scDotnetFiles(scNpgsqlCS),
		Run:   func(g scGen) string { return scDotnetRun },
	},
	{
		Database: scPostgres, Language: "shell", ID: "psql", Label: "psql (command line)",
		Summary: "The native client. Its TLS settings come from the PG* environment variables, which is how libpq itself is configured everywhere.",
		Runtime: scRuntimeShell, SysPackages: []string{"psql-client"},
		Files: func(g scGen) []scFile {
			return []scFile{scFileOf("crud.sh", "shell", scPgShell, g)}
		},
		Run: func(g scGen) string { return "bash crud.sh" },
	},
}

// scPgSSLMode is libpq's own word for the three postures, and every client in this file except
// node-postgres speaks it directly.
const scPgSSLMode = `{{if .Verify}}verify-full{{else if .Encrypted}}require{{else}}disable{{end}}`

// ------------------------------------------------------------------------------- Python

const scPsycopgPy = `{{.Header "# "}}

import psycopg

DATABASE = {{.Database | q}}
TABLE = {{.Table | q}}

# libpq keywords, as psycopg takes them. The superuser is the only role a DBCanvas
# PostgreSQL node provisions — an application of your own would create its own role
# and grant it what it needs.
CONN = {
    "host": {{.Target.Host | q}},
    "port": {{.Target.Port}},
    "user": {{.Target.User | q}},
    "password": {{.Target.Password | q}},
    "sslmode": "` + scPgSSLMode + `",
{{- if .Verify}}
    # verify-full checks the chain and that the certificate names this host.
    "sslrootcert": {{.CA | q}},
{{- end}}
{{- if .MTLS}}
    "sslcert": {{.ClientCert | q}},
    "sslkey": {{.ClientKey | q}},
{{- end}}
    "connect_timeout": 10,
}


def main():
{{- if .Ops.Schema}}
    # CREATE DATABASE cannot run inside a transaction, and cannot be run from the
    # database it is creating — so this is a separate, autocommitting connection to
    # the maintenance database.
    with psycopg.connect(**CONN, dbname="postgres", autocommit=True) as admin:
        exists = admin.execute("SELECT 1 FROM pg_database WHERE datname = %s", (DATABASE,)).fetchone()
        if not exists:
            admin.execute('CREATE DATABASE "' + DATABASE + '"')
            print("Created database " + DATABASE)

    with psycopg.connect(**CONN, dbname=DATABASE) as conn:
{{- else}}
    with psycopg.connect(**CONN, dbname="postgres") as conn:
{{- end}}
        with conn.cursor() as cur:
            cur.execute("SELECT version()")
            print("Connected to {}:{} - {}".format(CONN["host"], CONN["port"], cur.fetchone()[0].split(",")[0]))
{{- if .Ops.Schema}}

            cur.execute("""{{scCustomersDDLPostgres .Table}}""")
            conn.commit()
            print("Schema ready: {}.{}".format(DATABASE, TABLE))
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            print("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
            cur.execute(
                "INSERT INTO " + TABLE + " (name, email) VALUES (%s, %s) RETURNING id",
                ({{scDemoName | q}}, {{scDemoEmail | q}}),
            )
            customer_id = cur.fetchone()[0]
            conn.commit()
            print("Created customer {}: {}".format(customer_id, {{scDemoName | q}}))
{{- end}}
{{- if .Ops.Read}}

            print("READ")
            cur.execute("SELECT id, name, email, created_at FROM " + TABLE + " WHERE id = %s", (customer_id,))
            for row in cur.fetchall():
                print(" | ".join(str(c) for c in row))
{{- end}}
{{- if .Ops.Update}}

            print("UPDATE")
            cur.execute(
                "UPDATE " + TABLE + " SET email = %s WHERE id = %s",
                ({{scDemoNewEmail | q}}, customer_id),
            )
            conn.commit()
            print("Updated customer {} ({} row)".format(customer_id, cur.rowcount))
            cur.execute("SELECT id, name, email FROM " + TABLE + " WHERE id = %s", (customer_id,))
            for row in cur.fetchall():
                print(" | ".join(str(c) for c in row))
{{- end}}
{{- if .Ops.Delete}}

            print("DELETE")
            cur.execute("DELETE FROM " + TABLE + " WHERE id = %s", (customer_id,))
            conn.commit()
            print("Deleted customer {} ({} row)".format(customer_id, cur.rowcount))
            cur.execute("SELECT COUNT(*) FROM " + TABLE + " WHERE id = %s", (customer_id,))
            print("Rows with that id now: {}".format(cur.fetchone()[0]))
{{- end}}
    # Leaving the with block closes the connection, committing or rolling back first.
    print("{{.Scenario.Label}} example completed successfully.")


if __name__ == "__main__":
    main()
`

// ------------------------------------------------------------------------------- Node.js

const scPgJS = `{{.Header "// "}}

const { Client } = require('pg')
{{- if or .Verify .MTLS}}
const fs = require('node:fs')
{{- end}}

const DATABASE = {{.Database | q}}
const TABLE = {{.Table | q}}

const BASE = {
  host: {{.Target.Host | q}},
  port: {{.Target.Port}},
  user: {{.Target.User | q}},
  password: {{.Target.Password | q}},
  connectionTimeoutMillis: 10000,
{{- if .Verify}}
  // node-postgres does not read PG* variables for TLS: the CA goes in as bytes and
  // rejectUnauthorized is what makes this verified rather than merely encrypted.
  ssl: {
    ca: fs.readFileSync({{.CA | q}}),
    rejectUnauthorized: true,
{{- if .MTLS}}
    cert: fs.readFileSync({{.ClientCert | q}}),
    key: fs.readFileSync({{.ClientKey | q}}),
{{- end}}
  },
{{- else if .Encrypted}}
  ssl: { rejectUnauthorized: false },
{{- else}}
  ssl: false,
{{- end}}
}

async function main() {
{{- if .Ops.Schema}}
  // CREATE DATABASE needs a connection to a database that already exists, and cannot
  // be run inside a transaction.
  const admin = new Client({ ...BASE, database: 'postgres' })
  await admin.connect()
  const found = await admin.query('SELECT 1 FROM pg_database WHERE datname = $1', [DATABASE])
  if (found.rowCount === 0) {
    await admin.query(` + "`" + `CREATE DATABASE "${DATABASE}"` + "`" + `)
    console.log(` + "`" + `Created database ${DATABASE}` + "`" + `)
  }
  await admin.end()

  const client = new Client({ ...BASE, database: DATABASE })
{{- else}}
  const client = new Client({ ...BASE, database: 'postgres' })
{{- end}}
  await client.connect()
  try {
    const { rows: [v] } = await client.query('SELECT version()')
    console.log(` + "`" + `Connected to ${BASE.host}:${BASE.port} - ${v.version.split(',')[0]}` + "`" + `)
{{- if .Ops.Schema}}

    await client.query(` + "`" + `{{scCustomersDDLPostgres .Table}}` + "`" + `)
    console.log(` + "`" + `Schema ready: ${DATABASE}.${TABLE}` + "`" + `)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

    console.log('{{if .Ops.Create}}CREATE{{else}}SEED{{end}}')
    const ins = await client.query(
      ` + "`" + `INSERT INTO ${TABLE} (name, email) VALUES ($1, $2) RETURNING id` + "`" + `,
      [{{scDemoName | q}}, {{scDemoEmail | q}}],
    )
    const customerId = ins.rows[0].id
    console.log(` + "`" + `Created customer ${customerId}: {{scDemoName}}` + "`" + `)
{{- end}}
{{- if .Ops.Read}}

    console.log('READ')
    const read = await client.query(
      ` + "`" + `SELECT id, name, email, created_at FROM ${TABLE} WHERE id = $1` + "`" + `, [customerId])
    for (const r of read.rows) console.log([r.id, r.name, r.email, r.created_at.toISOString()].join(' | '))
{{- end}}
{{- if .Ops.Update}}

    console.log('UPDATE')
    const upd = await client.query(
      ` + "`" + `UPDATE ${TABLE} SET email = $1 WHERE id = $2` + "`" + `,
      [{{scDemoNewEmail | q}}, customerId],
    )
    console.log(` + "`" + `Updated customer ${customerId} (${upd.rowCount} row)` + "`" + `)
    const after = await client.query(` + "`" + `SELECT id, name, email FROM ${TABLE} WHERE id = $1` + "`" + `, [customerId])
    for (const r of after.rows) console.log([r.id, r.name, r.email].join(' | '))
{{- end}}
{{- if .Ops.Delete}}

    console.log('DELETE')
    const del = await client.query(` + "`" + `DELETE FROM ${TABLE} WHERE id = $1` + "`" + `, [customerId])
    console.log(` + "`" + `Deleted customer ${customerId} (${del.rowCount} row)` + "`" + `)
    const left = await client.query(` + "`" + `SELECT COUNT(*)::int AS n FROM ${TABLE} WHERE id = $1` + "`" + `, [customerId])
    console.log(` + "`" + `Rows with that id now: ${left.rows[0].n}` + "`" + `)
{{- end}}
  } finally {
    await client.end()
  }
  console.log('{{.Scenario.Label}} example completed successfully.')
}

main().catch((err) => { console.error(err); process.exit(1) })
`

// ------------------------------------------------------------------------------- Go

const scPgGo = `{{.Header "// "}}

package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	host     = {{.Target.Host | q}}
	port     = {{.Target.Port}}
	user     = {{.Target.User | q}}
	password = {{.Target.Password | q}}
	database = {{.Database | q}}
	table    = {{.Table | q}}
)

// dsn is a libpq keyword/value string; pgx accepts it unchanged, including the TLS
// keywords, which is why nothing here has to build a tls.Config by hand.
func dsn(dbname string) string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10{{if .Verify}} sslrootcert=%s{{end}}{{if .MTLS}} sslcert=%s sslkey=%s{{end}}",
		host, port, user, password, dbname, {{if .Verify}}"verify-full"{{else if .Encrypted}}"require"{{else}}"disable"{{end}},
{{- if .Verify}}
		{{.CA | q}},
{{- end}}
{{- if .MTLS}}
		{{.ClientCert | q}}, {{.ClientKey | q}},
{{- end}}
	)
}

func main() {
{{- if .Ops.Schema}}
	// CREATE DATABASE cannot run in a transaction and cannot be run from inside the
	// database being created: connect to the maintenance database first.
	admin, err := sql.Open("pgx", dsn("postgres"))
	if err != nil {
		log.Fatalf("open maintenance connection: %v", err)
	}
	var exists int
	switch err := admin.QueryRow("SELECT 1 FROM pg_database WHERE datname = $1", database).Scan(&exists); err {
	case nil:
	case sql.ErrNoRows:
		// A PostgreSQL identifier is quoted with double quotes, and the name here is
		// DBCanvas's own constant rather than anything a user typed.
		if _, err := admin.Exec("CREATE DATABASE \"" + database + "\""); err != nil {
			log.Fatalf("create database: %v", err)
		}
		fmt.Printf("Created database %s\n", database)
	default:
		log.Fatalf("look for the database: %v", err)
	}
	admin.Close()

	db, err := sql.Open("pgx", dsn(database))
{{- else}}
	db, err := sql.Open("pgx", dsn("postgres"))
{{- end}}
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRow("SELECT version()").Scan(&version); err != nil {
		log.Fatalf("connect: %v", err)
	}
	fmt.Printf("Connected to %s:%d - %s\n", host, port, strings.SplitN(version, ",", 2)[0])
{{- if .Ops.Schema}}

	if _, err := db.Exec(` + "`" + `{{scCustomersDDLPostgres .Table}}` + "`" + `); err != nil {
		log.Fatalf("create table: %v", err)
	}
	fmt.Printf("Schema ready: %s.%s\n", database, table)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

	fmt.Println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
	var customerID int64
	if err := db.QueryRow(
		"INSERT INTO "+table+" (name, email) VALUES ($1, $2) RETURNING id",
		{{scDemoName | q}}, {{scDemoEmail | q}},
	).Scan(&customerID); err != nil {
		log.Fatalf("insert: %v", err)
	}
	fmt.Printf("Created customer %d: %s\n", customerID, {{scDemoName | q}})
{{- end}}
{{- if .Ops.Read}}

	fmt.Println("READ")
	rows, err := db.Query("SELECT id, name, email FROM "+table+" WHERE id = $1", customerID)
	if err != nil {
		log.Fatalf("select: %v", err)
	}
	for rows.Next() {
		var id int64
		var name, email string
		if err := rows.Scan(&id, &name, &email); err != nil {
			log.Fatalf("scan: %v", err)
		}
		fmt.Printf("%d | %s | %s\n", id, name, email)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows: %v", err)
	}
	rows.Close()
{{- end}}
{{- if .Ops.Update}}

	fmt.Println("UPDATE")
	upd, err := db.Exec("UPDATE "+table+" SET email = $1 WHERE id = $2", {{scDemoNewEmail | q}}, customerID)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	n, _ := upd.RowsAffected()
	fmt.Printf("Updated customer %d (%d row)\n", customerID, n)
	var email string
	if err := db.QueryRow("SELECT email FROM "+table+" WHERE id = $1", customerID).Scan(&email); err != nil {
		log.Fatalf("re-read: %v", err)
	}
	fmt.Printf("%d | %s | %s\n", customerID, {{scDemoName | q}}, email)
{{- end}}
{{- if .Ops.Delete}}

	fmt.Println("DELETE")
	del, err := db.Exec("DELETE FROM "+table+" WHERE id = $1", customerID)
	if err != nil {
		log.Fatalf("delete: %v", err)
	}
	gone, _ := del.RowsAffected()
	fmt.Printf("Deleted customer %d (%d row)\n", customerID, gone)
	var left int
	if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE id = $1", customerID).Scan(&left); err != nil {
		log.Fatalf("count: %v", err)
	}
	fmt.Printf("Rows with that id now: %d\n", left)
{{- end}}

	fmt.Println("{{.Scenario.Label}} example completed successfully.")
}
`

// ------------------------------------------------------------------------------- Java

// scPgJDBCURL is pgJDBC's URL. sslrootcert takes a PEM path directly — the JDK's usual insistence
// on a keystore does not apply here, because the driver reads the file itself.
const scPgJDBCURL = `jdbc:postgresql://{{.Target.Host}}:{{.Target.Port}}/%s?sslmode=` + scPgSSLMode +
	`{{if .Verify}}&sslrootcert={{.CA}}{{end}}` +
	`{{if .MTLS}}&sslcert={{.ClientCert}}&sslkey={{.ClientKeyDER}}{{end}}` +
	`&connectTimeout=10&ApplicationName=dbcanvas-sample`

const scPgJDBCJava = `{{.Header "// "}}

import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;

public class DbCanvasCrud {

    private static final String USER = {{.Target.User | q}};
    private static final String PASSWORD = {{.Target.Password | q}};
    private static final String DATABASE = {{.Database | q}};
    private static final String TABLE = {{.Table | q}};

    private static String url(String database) {
        return String.format("` + scPgJDBCURL + `", database);
    }

    public static void main(String[] args) throws Exception {
{{- if .Ops.Schema}}
        // A JDBC URL names a database that has to exist already, so the demo database is
        // created from a connection to the maintenance database first. CREATE DATABASE
        // cannot run inside a transaction, which is why autocommit is left on.
        try (Connection admin = DriverManager.getConnection(url("postgres"), USER, PASSWORD)) {
            boolean exists;
            try (PreparedStatement ps = admin.prepareStatement("SELECT 1 FROM pg_database WHERE datname = ?")) {
                ps.setString(1, DATABASE);
                try (ResultSet rs = ps.executeQuery()) {
                    exists = rs.next();
                }
            }
            if (!exists) {
                try (Statement st = admin.createStatement()) {
                    st.executeUpdate("CREATE DATABASE \"" + DATABASE + "\"");
                }
                System.out.println("Created database " + DATABASE);
            }
        }

        try (Connection conn = DriverManager.getConnection(url(DATABASE), USER, PASSWORD)) {
{{- else}}
        try (Connection conn = DriverManager.getConnection(url("postgres"), USER, PASSWORD)) {
{{- end}}
            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("SELECT version()")) {
                rs.next();
                System.out.println("Connected to {{.Target.Host}}:{{.Target.Port}} - " + rs.getString(1).split(",")[0]);
            }
{{- if .Ops.Schema}}

            try (Statement st = conn.createStatement()) {
                st.executeUpdate("{{scCustomersDDLPostgresOneLine .Table}}");
            }
            System.out.println("Schema ready: " + DATABASE + "." + TABLE);
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            System.out.println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
            long customerId;
            try (PreparedStatement ps = conn.prepareStatement(
                    "INSERT INTO " + TABLE + " (name, email) VALUES (?, ?) RETURNING id")) {
                ps.setString(1, {{scDemoName | q}});
                ps.setString(2, {{scDemoEmail | q}});
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    customerId = rs.getLong(1);
                }
            }
            System.out.println("Created customer " + customerId + ": {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

            System.out.println("READ");
            try (PreparedStatement ps = conn.prepareStatement("SELECT id, name, email FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                try (ResultSet rs = ps.executeQuery()) {
                    while (rs.next()) {
                        System.out.println(rs.getLong("id") + " | " + rs.getString("name") + " | " + rs.getString("email"));
                    }
                }
            }
{{- end}}
{{- if .Ops.Update}}

            System.out.println("UPDATE");
            try (PreparedStatement ps = conn.prepareStatement("UPDATE " + TABLE + " SET email = ? WHERE id = ?")) {
                ps.setString(1, {{scDemoNewEmail | q}});
                ps.setLong(2, customerId);
                System.out.println("Updated customer " + customerId + " (" + ps.executeUpdate() + " row)");
            }
            try (PreparedStatement ps = conn.prepareStatement("SELECT email FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    System.out.println(customerId + " | {{scDemoName}} | " + rs.getString(1));
                }
            }
{{- end}}
{{- if .Ops.Delete}}

            System.out.println("DELETE");
            try (PreparedStatement ps = conn.prepareStatement("DELETE FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                System.out.println("Deleted customer " + customerId + " (" + ps.executeUpdate() + " row)");
            }
            try (PreparedStatement ps = conn.prepareStatement("SELECT COUNT(*) FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    System.out.println("Rows with that id now: " + rs.getInt(1));
                }
            }
{{- end}}
        }
        System.out.println("{{.Scenario.Label}} example completed successfully.");
    }
}
`

const scPgHikariJava = `{{.Header "// "}}

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;

import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;

public class DbCanvasCrud {

    private static final String USER = {{.Target.User | q}};
    private static final String PASSWORD = {{.Target.Password | q}};
    private static final String DATABASE = {{.Database | q}};
    private static final String TABLE = {{.Table | q}};

    // Lab defaults, not a tuning recommendation.
    private static final int MAX_POOL_SIZE = 5;
    private static final int MIN_IDLE = 1;

    private static String url(String database) {
        return String.format("` + scPgJDBCURL + `", database);
    }

    private static HikariDataSource pool(String database) {
        HikariConfig cfg = new HikariConfig();
        cfg.setJdbcUrl(url(database));
        cfg.setUsername(USER);
        cfg.setPassword(PASSWORD);
        cfg.setMaximumPoolSize(MAX_POOL_SIZE);
        cfg.setMinimumIdle(MIN_IDLE);
        cfg.setPoolName("dbcanvas-sample");
        cfg.setConnectionTimeout(10_000);
        cfg.setInitializationFailTimeout(10_000);
        return new HikariDataSource(cfg);
    }

    public static void main(String[] args) throws Exception {
{{- if .Ops.Schema}}
        // The bootstrap is one connection and one statement, so it is done with
        // DriverManager rather than by building a pool for the maintenance database.
        try (Connection admin = DriverManager.getConnection(url("postgres"), USER, PASSWORD)) {
            boolean exists;
            try (PreparedStatement ps = admin.prepareStatement("SELECT 1 FROM pg_database WHERE datname = ?")) {
                ps.setString(1, DATABASE);
                try (ResultSet rs = ps.executeQuery()) {
                    exists = rs.next();
                }
            }
            if (!exists) {
                try (Statement st = admin.createStatement()) {
                    st.executeUpdate("CREATE DATABASE \"" + DATABASE + "\"");
                }
                System.out.println("Created database " + DATABASE);
            }
        }

        try (HikariDataSource ds = pool(DATABASE)) {
{{- else}}
        try (HikariDataSource ds = pool("postgres")) {
{{- end}}
            try (Connection conn = ds.getConnection();
                 Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("SELECT version()")) {
                rs.next();
                System.out.println("Connected to {{.Target.Host}}:{{.Target.Port}} - " + rs.getString(1).split(",")[0]);
                System.out.println("Pool: maximumPoolSize=" + MAX_POOL_SIZE + ", minimumIdle=" + MIN_IDLE);
            }
{{- if .Ops.Schema}}

            try (Connection conn = ds.getConnection(); Statement st = conn.createStatement()) {
                st.executeUpdate("{{scCustomersDDLPostgresOneLine .Table}}");
            }
            System.out.println("Schema ready: " + DATABASE + "." + TABLE);
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            System.out.println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
            long customerId;
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement(
                     "INSERT INTO " + TABLE + " (name, email) VALUES (?, ?) RETURNING id")) {
                ps.setString(1, {{scDemoName | q}});
                ps.setString(2, {{scDemoEmail | q}});
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    customerId = rs.getLong(1);
                }
            }
            System.out.println("Created customer " + customerId + ": {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

            System.out.println("READ");
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement("SELECT id, name, email FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                try (ResultSet rs = ps.executeQuery()) {
                    while (rs.next()) {
                        System.out.println(rs.getLong("id") + " | " + rs.getString("name") + " | " + rs.getString("email"));
                    }
                }
            }
{{- end}}
{{- if .Ops.Update}}

            System.out.println("UPDATE");
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement("UPDATE " + TABLE + " SET email = ? WHERE id = ?")) {
                ps.setString(1, {{scDemoNewEmail | q}});
                ps.setLong(2, customerId);
                System.out.println("Updated customer " + customerId + " (" + ps.executeUpdate() + " row)");
            }
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement("SELECT email FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    System.out.println(customerId + " | {{scDemoName}} | " + rs.getString(1));
                }
            }
{{- end}}
{{- if .Ops.Delete}}

            System.out.println("DELETE");
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement("DELETE FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                System.out.println("Deleted customer " + customerId + " (" + ps.executeUpdate() + " row)");
            }
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement("SELECT COUNT(*) FROM " + TABLE + " WHERE id = ?")) {
                ps.setLong(1, customerId);
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    System.out.println("Rows with that id now: " + rs.getInt(1));
                }
            }
{{- end}}
        }
        System.out.println("{{.Scenario.Label}} example completed successfully.");
    }
}
`

// ------------------------------------------------------------------------------- shell

const scPgShell = `#!/usr/bin/env bash
{{.Header "# "}}

set -euo pipefail

HOST={{.Target.Host | sq}}
PORT={{.Target.Port}}
DATABASE={{.Database | sq}}
TABLE={{.Table | sq}}

# libpq reads all of these itself, which keeps the password off the command line and
# every TLS setting in one place.
export PGHOST="$HOST"
export PGPORT="$PORT"
export PGUSER={{.Target.User | sq}}
export PGPASSWORD={{.Target.Password | sq}}
export PGCONNECT_TIMEOUT=10
export PGSSLMODE=` + scPgSSLMode + `
{{- if .Verify}}
export PGSSLROOTCERT={{.CA | sq}}
{{- end}}
{{- if .MTLS}}
export PGSSLCERT={{.ClientCert | sq}}
export PGSSLKEY={{.ClientKey | sq}}
{{- end}}

# On EL the Percona PostgreSQL client is under /usr/pgsql-NN/bin and not on PATH; on
# Debian it is. Resolve it rather than assume.
PSQL=$(command -v psql || echo {{.PgBinDir | sq}}/psql)

# -q matters more than it looks: without it psql prints the command tag ("INSERT 0 1")
# after the rows, so $(...) around an INSERT ... RETURNING id captures two lines and the
# next statement is built from a broken id. ON_ERROR_STOP makes a failed statement a
# non-zero exit, which is what set -e is waiting for.
RUN=("$PSQL" -q -v ON_ERROR_STOP=1)

echo "Connecting to $HOST:$PORT..."
"${RUN[@]}" -d postgres -Atc 'SELECT version()' | cut -d, -f1
{{- if .Ops.Schema}}

if [ "$("${RUN[@]}" -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = '$DATABASE'")" != "1" ]; then
  "${RUN[@]}" -d postgres -Atc "CREATE DATABASE \"$DATABASE\""
  echo "Created database $DATABASE"
fi
"${RUN[@]}" -d "$DATABASE" <<SQL
{{scCustomersDDLPostgres .Table}};
SQL
echo "Schema ready: $DATABASE.$TABLE"
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

echo "{{if .Ops.Create}}CREATE{{else}}SEED{{end}}"
ID=$("${RUN[@]}" -d "$DATABASE" -Atc "INSERT INTO $TABLE (name, email) VALUES ({{scDemoName | sq}}, {{scDemoEmail | sq}}) RETURNING id")
echo "Created customer $ID: {{scDemoName}}"
{{- end}}
{{- if .Ops.Read}}

echo "READ"
"${RUN[@]}" -d "$DATABASE" -At -F ' | ' -c "SELECT id, name, email FROM $TABLE WHERE id = $ID"
{{- end}}
{{- if .Ops.Update}}

echo "UPDATE"
"${RUN[@]}" -d "$DATABASE" -Atc "UPDATE $TABLE SET email = {{scDemoNewEmail | sq}} WHERE id = $ID" >/dev/null
echo "Updated customer $ID"
"${RUN[@]}" -d "$DATABASE" -At -F ' | ' -c "SELECT id, name, email FROM $TABLE WHERE id = $ID"
{{- end}}
{{- if .Ops.Delete}}

echo "DELETE"
"${RUN[@]}" -d "$DATABASE" -Atc "DELETE FROM $TABLE WHERE id = $ID" >/dev/null
echo "Deleted customer $ID"
echo "Rows with that id now: $("${RUN[@]}" -d "$DATABASE" -Atc "SELECT COUNT(*) FROM $TABLE WHERE id = $ID")"
{{- end}}

echo "{{.Scenario.Label}} example completed successfully."
`

// ------------------------------------------------------------------------------- C#

const scNpgsqlCS = `{{.Header "// "}}

using Npgsql;
{{- if .Ops.Any}}

const string Database = {{.Database | cs}};
const string Table = {{.Table | cs}};
{{- end}}

// Npgsql's connection string keywords. The superuser is the only role a DBCanvas
// PostgreSQL node provisions — an application of your own would create its own role
// and grant it what it needs.
NpgsqlConnectionStringBuilder Settings(string database) => new()
{
    Host = {{.Target.Host | cs}},
    Port = {{.Target.Port}},
    Username = {{.Target.User | cs}},
    Password = {{.Target.Password | cs}},
    Database = database,
    Timeout = 10,
{{- if .Verify}}
    // VerifyFull checks the chain against this CA and that the certificate names this host.
    SslMode = SslMode.VerifyFull,
    RootCertificate = {{.CA | cs}},
{{- else if .Encrypted}}
    // Require encrypts without checking who answered, as libpq's own require does.
    SslMode = SslMode.Require,
{{- else}}
    SslMode = SslMode.Disable,
{{- end}}
{{- if .MTLS}}
    SslCertificate = {{.ClientCert | cs}},
    SslKey = {{.ClientKey | cs}},
{{- end}}
};
{{- if .Ops.Schema}}

// CREATE DATABASE cannot run inside a transaction, and cannot be run from the database
// it is creating — so this is a separate connection to the maintenance database.
await using (var admin = new NpgsqlConnection(Settings("postgres").ConnectionString))
{
    await admin.OpenAsync();
    await using var find = new NpgsqlCommand("SELECT 1 FROM pg_database WHERE datname = @name", admin);
    find.Parameters.AddWithValue("name", Database);
    if (await find.ExecuteScalarAsync() is null)
    {
        await using var create = new NpgsqlCommand("CREATE DATABASE \"" + Database + "\"", admin);
        await create.ExecuteNonQueryAsync();
        Console.WriteLine($"Created database {Database}");
    }
}

await using var conn = new NpgsqlConnection(Settings(Database).ConnectionString);
{{- else}}

await using var conn = new NpgsqlConnection(Settings("postgres").ConnectionString);
{{- end}}
await conn.OpenAsync();

await using (var cmd = new NpgsqlCommand("SELECT version()", conn))
{
    var version = (string)(await cmd.ExecuteScalarAsync())!;
    Console.WriteLine($"Connected to {conn.Host}:{conn.Port} - {version.Split(',')[0]}");
}
{{- if .Ops.Schema}}

await using (var cmd = new NpgsqlCommand(@"{{scCustomersDDLPostgres .Table}}", conn))
{
    await cmd.ExecuteNonQueryAsync();
}
Console.WriteLine($"Schema ready: {Database}.{Table}");
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

Console.WriteLine("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
int customerId;
await using (var cmd = new NpgsqlCommand("INSERT INTO " + Table + " (name, email) VALUES (@name, @email) RETURNING id", conn))
{
    cmd.Parameters.AddWithValue("name", {{scDemoName | cs}});
    cmd.Parameters.AddWithValue("email", {{scDemoEmail | cs}});
    customerId = (int)(await cmd.ExecuteScalarAsync())!;
}
Console.WriteLine($"Created customer {customerId}: {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

Console.WriteLine("READ");
await using (var cmd = new NpgsqlCommand("SELECT id, name, email, created_at FROM " + Table + " WHERE id = @id", conn))
{
    cmd.Parameters.AddWithValue("id", customerId);
    await using var rows = await cmd.ExecuteReaderAsync();
    while (await rows.ReadAsync())
    {
        Console.WriteLine($"{rows.GetInt32(0)} | {rows.GetString(1)} | {rows.GetString(2)} | {rows.GetDateTime(3):O}");
    }
}
{{- end}}
{{- if .Ops.Update}}

Console.WriteLine("UPDATE");
await using (var cmd = new NpgsqlCommand("UPDATE " + Table + " SET email = @email WHERE id = @id", conn))
{
    cmd.Parameters.AddWithValue("email", {{scDemoNewEmail | cs}});
    cmd.Parameters.AddWithValue("id", customerId);
    Console.WriteLine($"Updated customer {customerId} ({await cmd.ExecuteNonQueryAsync()} row)");
}
await using (var cmd = new NpgsqlCommand("SELECT id, name, email FROM " + Table + " WHERE id = @id", conn))
{
    cmd.Parameters.AddWithValue("id", customerId);
    await using var rows = await cmd.ExecuteReaderAsync();
    while (await rows.ReadAsync())
    {
        Console.WriteLine($"{rows.GetInt32(0)} | {rows.GetString(1)} | {rows.GetString(2)}");
    }
}
{{- end}}
{{- if .Ops.Delete}}

Console.WriteLine("DELETE");
await using (var cmd = new NpgsqlCommand("DELETE FROM " + Table + " WHERE id = @id", conn))
{
    cmd.Parameters.AddWithValue("id", customerId);
    Console.WriteLine($"Deleted customer {customerId} ({await cmd.ExecuteNonQueryAsync()} row)");
}
await using (var cmd = new NpgsqlCommand("SELECT COUNT(*) FROM " + Table + " WHERE id = @id", conn))
{
    cmd.Parameters.AddWithValue("id", customerId);
    Console.WriteLine($"Rows with that id now: {await cmd.ExecuteScalarAsync()}");
}
{{- end}}

Console.WriteLine("{{.Scenario.Label}} example completed successfully.");
`
