package main

// samplecode_mysql.go — the MySQL-family samples: Percona Server, PXC, MySQL Community and
// MariaDB, which are one wire protocol and one set of drivers as far as a client is concerned.
//
// Two Python drivers are offered on purpose. mysql-connector-python is Oracle's own and is
// GPL-2.0 with the Universal FOSS Exception — the same licence position as Connector/J, and the
// same one DBCanvas already documents for the Ledger Sim image. PyMySQL is MIT and pure Python,
// which makes it the one to reach for when that exception is not something you want to depend on.
// Both are installed, never vendored, so neither licence reaches this repository; the note on the
// dependency says so where a user will see it.

var scMySQLClients = []scClient{
	{
		Database: scMySQL, Language: "python", ID: "mysql-connector", Label: "mysql-connector-python",
		Summary: "Oracle's own Python driver. Pure Python with an optional C extension, and the one MySQL's documentation is written against.",
		Runtime: scRuntimePython,
		Deps: []scDep{{
			Manager: "pip", Name: "mysql-connector-python", Import: "mysql.connector",
			License: "GPL-2.0-only WITH Universal-FOSS-Exception-1.0",
			URL:     "https://github.com/mysql/mysql-connector-python",
			Note:    "GPLv2-only and GPLv3 are not compatible on their own; Oracle's Universal FOSS Exception is what permits this driver to be used inside a larger work under another free licence. DBCanvas installs it from PyPI at run time and does not redistribute it.",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.py", "python", scMySQLConnectorPy, g),
				scFileOf("requirements.txt", "text", "mysql-connector-python\n", g),
			}
		},
		Run: func(g scGen) string { return scVenv + "/bin/python crud.py" },
	},
	{
		Database: scMySQL, Language: "python", ID: "pymysql", Label: "PyMySQL",
		Summary: "A pure-Python MySQL client under the MIT licence — no compiler, no C library, and no licence exception to reason about.",
		Runtime: scRuntimePython,
		Deps: []scDep{{
			Manager: "pip", Name: "PyMySQL", Import: "pymysql",
			License: "MIT", URL: "https://github.com/PyMySQL/PyMySQL",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.py", "python", scPyMySQLPy, g),
				scFileOf("requirements.txt", "text", "PyMySQL\n", g),
			}
		},
		Run: func(g scGen) string { return scVenv + "/bin/python crud.py" },
	},
	{
		Database: scMySQL, Language: "node", ID: "mysql2", Label: "mysql2",
		Summary: "The Node.js MySQL driver with promise and prepared-statement support. The samples use its promise API.",
		Runtime: scRuntimeNode,
		Deps: []scDep{{
			Manager: "npm", Name: "mysql2", Version: "^3", License: "MIT",
			URL: "https://github.com/sidorares/node-mysql2",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.js", "javascript", scMySQL2JS, g),
				scFileOf("package.json", "json", scNodePackageJSON, g),
			}
		},
		Run: func(g scGen) string { return "node crud.js" },
	},
	{
		Database: scMySQL, Language: "go", ID: "database-sql", Label: "database/sql + go-sql-driver/mysql",
		Summary: "Go's own database/sql interface over the standard community MySQL driver — the pairing almost every Go service uses.",
		Runtime: scRuntimeGo,
		Deps: []scDep{{
			Manager: "gomod", Name: "github.com/go-sql-driver/mysql", Version: "v1.10.0",
			License: "MPL-2.0", URL: "https://github.com/go-sql-driver/mysql",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("main.go", "go", scMySQLGo, g),
				scFileOf("go.mod", "text", scGoMod, g),
			}
		},
		Run: func(g scGen) string { return "go run ." },
	},
	{
		Database: scMySQL, Language: "java", ID: "jdbc", Label: "JDBC (MySQL Connector/J)",
		Summary: "Plain JDBC: a DriverManager connection, prepared statements, try-with-resources. No pool, so every operation opens and closes its own connection.",
		Runtime: scRuntimeJava,
		Deps: []scDep{{
			Manager: "maven", Name: "com.mysql:mysql-connector-j", Version: "9.3.0",
			License: "GPL-2.0-only WITH Universal-FOSS-Exception-1.0",
			URL:     "https://github.com/mysql/mysql-connector-j",
			Note:    "Usable inside another free-licensed work through Oracle's Universal FOSS Exception. Maven fetches it at build time; DBCanvas neither ships nor relicenses it.",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("src/main/java/DbCanvasCrud.java", "java", scMySQLJDBCJava, g),
				scFileOf("pom.xml", "xml", scMavenPOM, g),
			}
		},
		Run: func(g scGen) string { return scMavenRun },
	},
	{
		Database: scMySQL, Language: "java", ID: "hikari", Label: "JDBC + HikariCP",
		Summary: "The same JDBC driver behind a HikariCP pool: connections are borrowed and returned rather than opened and closed, and the pool is what the program shuts down at the end.",
		Runtime: scRuntimeJava,
		Deps: []scDep{
			{
				Manager: "maven", Name: "com.mysql:mysql-connector-j", Version: "9.3.0",
				License: "GPL-2.0-only WITH Universal-FOSS-Exception-1.0",
				URL:     "https://github.com/mysql/mysql-connector-j",
				Note:    "Usable inside another free-licensed work through Oracle's Universal FOSS Exception. Maven fetches it at build time; DBCanvas neither ships nor relicenses it.",
			},
			{Manager: "maven", Name: "com.zaxxer:HikariCP", Version: "6.3.0", License: "Apache-2.0", URL: "https://github.com/brettwooldridge/HikariCP"},
			{
				Manager: "maven", Name: "org.slf4j:slf4j-api", Version: "2.0.19", License: "MIT",
				URL:  "https://www.slf4j.org/",
				Note: "Declared explicitly, not left to HikariCP: HikariCP 6.x still brings slf4j-api 1.7.x transitively, and a 1.7 API with a 2.0 binding finds no provider and logs nothing but a warning about it.",
			},
			{
				Manager: "maven", Name: "org.slf4j:slf4j-simple", Version: "2.0.19", License: "MIT",
				URL:  "https://www.slf4j.org/",
				Note: "HikariCP logs through SLF4J and needs a binding. slf4j-simple is MIT; Logback is EPL-1.0/LGPL-2.1 dual-licensed and EPL-1.0 is GPL-incompatible, which is why it is not the one here.",
			},
		},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("src/main/java/DbCanvasCrud.java", "java", scMySQLHikariJava, g),
				scFileOf("pom.xml", "xml", scMavenPOM, g),
			}
		},
		Run: func(g scGen) string { return scMavenRun },
	},
	{
		Database: scMySQL, Language: "shell", ID: "mysql", Label: "mysql (command line)",
		Summary: "The native client, driven from a shell script — the shortest path from this node to a prompt, and the first thing to reach for when a driver disagrees with a server.",
		Runtime: scRuntimeShell, SysPackages: []string{"mysql-client"},
		Files: func(g scGen) []scFile {
			return []scFile{scFileOf("crud.sh", "shell", scMySQLShell, g)}
		},
		Run: func(g scGen) string { return "bash crud.sh" },
	},
}

// ------------------------------------------------------------------------------- Python

const scMySQLConnectorPy = `{{.Header "# "}}

import mysql.connector

DATABASE = {{.Database | q}}
TABLE = {{.Table | q}}

# Connection settings, straight from the deployment on the canvas.
CONFIG = {
    "host": {{.Target.Host | q}},
    "port": {{.Target.Port}},
    "user": {{.Target.User | q}},
    "password": {{.Target.Password | q}},
{{- if .Verify}}
    # The server's certificate is signed by the stack CA, which is already in this
    # node's trust store — so both the chain and the hostname can be checked.
    "ssl_ca": {{.CA | q}},
    "ssl_verify_cert": True,
    "ssl_verify_identity": True,
{{- else if .Encrypted}}
    # Encrypted, but not verified: this server has only the certificate it generated
    # for itself, so there is nothing to check it against.
    "ssl_disabled": False,
    "ssl_verify_cert": False,
{{- else}}
    "ssl_disabled": True,
{{- end}}
{{- if .MTLS}}
    # Mutual TLS: the certificate this client presents, issued by the stack CA.
    "ssl_cert": {{.ClientCert | q}},
    "ssl_key": {{.ClientKey | q}},
{{- end}}
}


def main():
    conn = mysql.connector.connect(**CONFIG)
    try:
        cur = conn.cursor()
        cur.execute("SELECT VERSION()")
        print("Connected to {}:{} - MySQL {}".format(CONFIG["host"], CONFIG["port"], cur.fetchone()[0]))
{{- if .Ops.Schema}}

        cur.execute("CREATE DATABASE IF NOT EXISTS " + DATABASE)
        conn.database = DATABASE
        cur.execute("""{{scCustomersDDLMySQL .Table}}""")
        conn.commit()
        print("Schema ready: {}.{}".format(DATABASE, TABLE))
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

        print("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
        cur.execute(
            "INSERT INTO " + TABLE + " (name, email) VALUES (%s, %s)",
            ({{scDemoName | q}}, {{scDemoEmail | q}}),
        )
        conn.commit()
        customer_id = cur.lastrowid
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

        cur.close()
    finally:
        # Always give the connection back, on the way out of an exception as well.
        conn.close()
    print("{{.Scenario.Label}} example completed successfully.")


if __name__ == "__main__":
    main()
`

const scPyMySQLPy = `{{.Header "# "}}

import pymysql

DATABASE = {{.Database | q}}
TABLE = {{.Table | q}}

CONFIG = {
    "host": {{.Target.Host | q}},
    "port": {{.Target.Port}},
    "user": {{.Target.User | q}},
    "password": {{.Target.Password | q}},
    "autocommit": False,
{{- if .Verify}}
    # PyMySQL takes a dict of ssl options; check_hostname is what makes this
    # verify-full rather than merely encrypted.
    "ssl": {
        "ca": {{.CA | q}},
        "check_hostname": True,
{{- if .MTLS}}
        "cert": {{.ClientCert | q}},
        "key": {{.ClientKey | q}},
{{- end}}
    },
{{- else if .Encrypted}}
    # An empty ssl dict asks for TLS without verifying the server, which is all
    # a self-signed server certificate allows.
    "ssl": {
        "check_hostname": False,
{{- if .MTLS}}
        "cert": {{.ClientCert | q}},
        "key": {{.ClientKey | q}},
{{- end}}
    },
{{- end}}
}


def main():
    conn = pymysql.connect(**CONFIG)
    try:
        with conn.cursor() as cur:
            cur.execute("SELECT VERSION()")
            print("Connected to {}:{} - MySQL {}".format(CONFIG["host"], CONFIG["port"], cur.fetchone()[0]))
{{- if .Ops.Schema}}

            cur.execute("CREATE DATABASE IF NOT EXISTS " + DATABASE)
            cur.execute("USE " + DATABASE)
            cur.execute("""{{scCustomersDDLMySQL .Table}}""")
            conn.commit()
            print("Schema ready: {}.{}".format(DATABASE, TABLE))
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            print("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
            cur.execute(
                "INSERT INTO " + TABLE + " (name, email) VALUES (%s, %s)",
                ({{scDemoName | q}}, {{scDemoEmail | q}}),
            )
            conn.commit()
            customer_id = cur.lastrowid
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
    finally:
        conn.close()
    print("{{.Scenario.Label}} example completed successfully.")


if __name__ == "__main__":
    main()
`

// ------------------------------------------------------------------------------- Node.js

const scNodePackageJSON = `{
  "name": "dbcanvas-sample",
  "private": true,
  "type": "commonjs",
  "description": "DBCanvas generated sample {{.SampleID}}",
  "main": "crud.js",
  "scripts": { "start": "node crud.js" },
  "dependencies": {
{{- range $i, $d := .Deps}}{{if $i}},{{end}}
    {{$d.Name | q}}: {{if $d.Version}}{{$d.Version | q}}{{else}}"*"{{end}}
{{- end}}
  }
}
`

const scMySQL2JS = `{{.Header "// "}}

const mysql = require('mysql2/promise')
{{- if or .Verify .MTLS}}
const fs = require('node:fs')
{{- end}}

const DATABASE = {{.Database | q}}
const TABLE = {{.Table | q}}

const CONFIG = {
  host: {{.Target.Host | q}},
  port: {{.Target.Port}},
  user: {{.Target.User | q}},
  password: {{.Target.Password | q}},
{{- if .Verify}}
  // rejectUnauthorized is the whole difference between encrypted and verified.
  ssl: {
    ca: fs.readFileSync({{.CA | q}}),
    rejectUnauthorized: true,
{{- if .MTLS}}
    cert: fs.readFileSync({{.ClientCert | q}}),
    key: fs.readFileSync({{.ClientKey | q}}),
{{- end}}
  },
{{- else if .Encrypted}}
  // Encrypted but unverified: the server has only a self-signed certificate.
  ssl: {
    rejectUnauthorized: false,
{{- if .MTLS}}
    cert: fs.readFileSync({{.ClientCert | q}}),
    key: fs.readFileSync({{.ClientKey | q}}),
{{- end}}
  },
{{- end}}
}

async function main() {
  const conn = await mysql.createConnection(CONFIG)
  try {
    const [[version]] = await conn.query('SELECT VERSION() AS v')
    console.log(` + "`" + `Connected to ${CONFIG.host}:${CONFIG.port} - MySQL ${version.v}` + "`" + `)
{{- if .Ops.Schema}}

    await conn.query(` + "`" + `CREATE DATABASE IF NOT EXISTS ${DATABASE}` + "`" + `)
    await conn.changeUser({ database: DATABASE })
    await conn.query(` + "`" + `{{scCustomersDDLMySQL .Table}}` + "`" + `)
    console.log(` + "`" + `Schema ready: ${DATABASE}.${TABLE}` + "`" + `)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

    console.log('{{if .Ops.Create}}CREATE{{else}}SEED{{end}}')
    const [ins] = await conn.execute(
      ` + "`" + `INSERT INTO ${TABLE} (name, email) VALUES (?, ?)` + "`" + `,
      [{{scDemoName | q}}, {{scDemoEmail | q}}],
    )
    const customerId = ins.insertId
    console.log(` + "`" + `Created customer ${customerId}: {{scDemoName}}` + "`" + `)
{{- end}}
{{- if .Ops.Read}}

    console.log('READ')
    const [rows] = await conn.execute(
      ` + "`" + `SELECT id, name, email, created_at FROM ${TABLE} WHERE id = ?` + "`" + `, [customerId])
    for (const r of rows) console.log([r.id, r.name, r.email, r.created_at.toISOString()].join(' | '))
{{- end}}
{{- if .Ops.Update}}

    console.log('UPDATE')
    const [upd] = await conn.execute(
      ` + "`" + `UPDATE ${TABLE} SET email = ? WHERE id = ?` + "`" + `,
      [{{scDemoNewEmail | q}}, customerId],
    )
    console.log(` + "`" + `Updated customer ${customerId} (${upd.affectedRows} row)` + "`" + `)
    const [after] = await conn.execute(` + "`" + `SELECT id, name, email FROM ${TABLE} WHERE id = ?` + "`" + `, [customerId])
    for (const r of after) console.log([r.id, r.name, r.email].join(' | '))
{{- end}}
{{- if .Ops.Delete}}

    console.log('DELETE')
    const [del] = await conn.execute(` + "`" + `DELETE FROM ${TABLE} WHERE id = ?` + "`" + `, [customerId])
    console.log(` + "`" + `Deleted customer ${customerId} (${del.affectedRows} row)` + "`" + `)
    const [[left]] = await conn.execute(` + "`" + `SELECT COUNT(*) AS n FROM ${TABLE} WHERE id = ?` + "`" + `, [customerId])
    console.log(` + "`" + `Rows with that id now: ${left.n}` + "`" + `)
{{- end}}
  } finally {
    await conn.end()
  }
  console.log('{{.Scenario.Label}} example completed successfully.')
}

main().catch((err) => { console.error(err); process.exit(1) })
`

// ------------------------------------------------------------------------------- Go

// scGoMod is the module file for every Go sample. The Go directive is deliberately modest: a
// newer toolchain reads an older module fine, and pinning it high would break the Go that ships
// with the older base images for no gain.
const scGoMod = `{{.Header "// "}}

module dbcanvas/sample

go 1.21

require (
{{- range .Deps}}
	{{.Name}} {{.Version}}
{{- end}}
)
`

const scMySQLGo = `{{.Header "// "}}

package main

import (
{{- if or .Verify .MTLS}}
	"crypto/tls"
{{- end}}
{{- if .Verify}}
	"crypto/x509"
{{- end}}
	"database/sql"
	"fmt"
	"log"
{{- if .Verify}}
	"os"
{{- end}}

	"github.com/go-sql-driver/mysql"
)

const (
	host     = {{.Target.Host | q}}
	port     = {{.Target.Port}}
	user     = {{.Target.User | q}}
	password = {{.Target.Password | q}}
	database = {{.Database | q}}
	table    = {{.Table | q}}
)

func main() {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = user, password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", host, port)
	cfg.ParseTime = true
{{- if or .Verify .MTLS}}

	// go-sql-driver takes a *registered* TLS config by name rather than a DSN parameter,
	// so anything beyond on/off is built here and referred to by that name.
	tlsCfg := &tls.Config{ServerName: host}
{{- if .Verify}}
	// RootCAs plus ServerName is what makes this verified rather than only encrypted.
	pool := x509.NewCertPool()
	ca, err := os.ReadFile({{.CA | q}})
	if err != nil {
		log.Fatalf("read CA: %v", err)
	}
	if !pool.AppendCertsFromPEM(ca) {
		log.Fatal("the CA file held no certificate")
	}
	tlsCfg.RootCAs = pool
{{- else}}
	// The server has only the certificate it generated for itself, so there is nothing
	// to check it against: encrypted, unverified.
	tlsCfg.InsecureSkipVerify = true
{{- end}}
{{- if .MTLS}}
	clientCert, cerr := tls.LoadX509KeyPair({{.ClientCert | q}}, {{.ClientKey | q}})
	if cerr != nil {
		log.Fatalf("read client certificate: %v", cerr)
	}
	tlsCfg.Certificates = []tls.Certificate{clientCert}
{{- end}}
	if err := mysql.RegisterTLSConfig("dbcanvas", tlsCfg); err != nil {
		log.Fatalf("register TLS config: %v", err)
	}
	cfg.TLSConfig = "dbcanvas"
{{- else if .Encrypted}}

	// skip-verify: encrypted, but the server is not identified — which is all a
	// self-signed server certificate supports.
	cfg.TLSConfig = "skip-verify"
{{- end}}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
		log.Fatalf("connect: %v", err)
	}
	fmt.Printf("Connected to %s:%d - MySQL %s\n", host, port, version)
{{- if .Ops.Schema}}

	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + database); err != nil {
		log.Fatalf("create database: %v", err)
	}
	// A pool hands out whichever connection is free, so USE cannot be relied on:
	// the database belongs in the DSN, which means a second handle once it exists.
	cfg.DBName = database
	db.Close()
	if db, err = sql.Open("mysql", cfg.FormatDSN()); err != nil {
		log.Fatalf("reopen on %s: %v", database, err)
	}
	if _, err := db.Exec(` + "`" + `{{scCustomersDDLMySQL .Table}}` + "`" + `); err != nil {
		log.Fatalf("create table: %v", err)
	}
	fmt.Printf("Schema ready: %s.%s\n", database, table)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

	fmt.Println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
	res, err := db.Exec("INSERT INTO "+table+" (name, email) VALUES (?, ?)", {{scDemoName | q}}, {{scDemoEmail | q}})
	if err != nil {
		log.Fatalf("insert: %v", err)
	}
	customerID, err := res.LastInsertId()
	if err != nil {
		log.Fatalf("last insert id: %v", err)
	}
	fmt.Printf("Created customer %d: %s\n", customerID, {{scDemoName | q}})
{{- end}}
{{- if .Ops.Read}}

	fmt.Println("READ")
	rows, err := db.Query("SELECT id, name, email FROM "+table+" WHERE id = ?", customerID)
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
	upd, err := db.Exec("UPDATE "+table+" SET email = ? WHERE id = ?", {{scDemoNewEmail | q}}, customerID)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	n, _ := upd.RowsAffected()
	fmt.Printf("Updated customer %d (%d row)\n", customerID, n)
	var email string
	if err := db.QueryRow("SELECT email FROM "+table+" WHERE id = ?", customerID).Scan(&email); err != nil {
		log.Fatalf("re-read: %v", err)
	}
	fmt.Printf("%d | %s | %s\n", customerID, {{scDemoName | q}}, email)
{{- end}}
{{- if .Ops.Delete}}

	fmt.Println("DELETE")
	del, err := db.Exec("DELETE FROM "+table+" WHERE id = ?", customerID)
	if err != nil {
		log.Fatalf("delete: %v", err)
	}
	gone, _ := del.RowsAffected()
	fmt.Printf("Deleted customer %d (%d row)\n", customerID, gone)
	var left int
	if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE id = ?", customerID).Scan(&left); err != nil {
		log.Fatalf("count: %v", err)
	}
	fmt.Printf("Rows with that id now: %d\n", left)
{{- end}}

	fmt.Println("{{.Scenario.Label}} example completed successfully.")
}
`

// ------------------------------------------------------------------------------- Java

// scMavenRun is how every Java sample is executed: compile with Maven, then run the class through
// the exec plugin so the project's own dependency classpath is the one the JVM gets.
const scMavenRun = "mvn -B -q compile exec:java -Dexec.mainClass=DbCanvasCrud"

// scMavenPOM is the project file for every Java sample. Deliberately minimal — a compiler plugin,
// an exec plugin and the dependencies the sample declares. No framework, no parent POM, nothing
// generated that the program does not use.
const scMavenPOM = `<?xml version="1.0" encoding="UTF-8"?>
<!--
{{xmlComment (.Header "  ")}}
-->
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>dev.dbcanvas.sample</groupId>
  <artifactId>dbcanvas-sample</artifactId>
  <version>1.0-SNAPSHOT</version>
  <packaging>jar</packaging>

  <properties>
    <maven.compiler.release>17</maven.compiler.release>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  </properties>

  <dependencies>
{{- range .Deps}}
{{- if eq .Manager "maven"}}
    <!-- {{.License}} — {{.URL}} -->
    <dependency>
      <groupId>{{index (splitCoord .Name) 0}}</groupId>
      <artifactId>{{index (splitCoord .Name) 1}}</artifactId>
      <version>{{.Version}}</version>
    </dependency>
{{- end}}
{{- end}}
  </dependencies>

  <build>
    <plugins>
      <!-- These two are pinned to the newest releases that still run on Maven 3.5, which is
           what EL8 ships and cannot be upgraded from: its maven:3.8 module stream installs a
           launcher whose jars symlink into the 3.5-era maven-resolver rpm, and the result
           cannot start at all. A newer compiler plugin buys nothing here - the release level
           below is what decides the bytecode - and costs every EL8 node. -->
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-compiler-plugin</artifactId>
        <version>3.8.1</version>
      </plugin>
      <plugin>
        <groupId>org.codehaus.mojo</groupId>
        <artifactId>exec-maven-plugin</artifactId>
        <version>3.1.0</version>
      </plugin>
    </plugins>
  </build>
</project>
`

// scMySQLJDBCURL is the Connector/J URL, built the way the driver documents it.
//
// Three properties are worth knowing about, and the generated comment says so in the file:
// createDatabaseIfNotExist, because a JDBC URL otherwise names a database that must already exist;
// sslMode, which is Connector/J's own vocabulary and does not match libmysqlclient's; and
// allowPublicKeyRetrieval, which caching_sha2_password needs when the connection is *not*
// encrypted — leave it out on a plaintext connection and the first login fails with an error that
// says nothing about keys.
const scMySQLJDBCURL = `jdbc:mysql://{{.Target.Host}}:{{.Target.Port}}/{{if .Ops.Schema}}{{.Database}}?createDatabaseIfNotExist=true&{{else}}?{{end}}` +
	`{{if .Verify}}sslMode=VERIFY_IDENTITY&trustCertificateKeyStoreUrl=file:{{.Truststore}}&trustCertificateKeyStoreType=PKCS12&trustCertificateKeyStorePassword={{.StorePass}}` +
	`{{else if .Encrypted}}sslMode=REQUIRED&allowPublicKeyRetrieval=true` +
	`{{else}}sslMode=DISABLED&allowPublicKeyRetrieval=true{{end}}` +
	`{{if .MTLS}}&clientCertificateKeyStoreUrl=file:{{.Keystore}}&clientCertificateKeyStoreType=PKCS12&clientCertificateKeyStorePassword={{.StorePass}}{{end}}` +
	`&connectTimeout=10000`

const scMySQLJDBCJava = `{{.Header "// "}}

import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;

public class DbCanvasCrud {

    // Built from the deployment: the host and port the Intranet publishes, and the TLS
    // properties that match how this server was actually set up.
    private static final String URL = "` + scMySQLJDBCURL + `";
    private static final String USER = {{.Target.User | q}};
    private static final String PASSWORD = {{.Target.Password | q}};
    private static final String TABLE = {{.Table | q}};

    public static void main(String[] args) throws Exception {
        try (Connection conn = DriverManager.getConnection(URL, USER, PASSWORD)) {
            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("SELECT VERSION()")) {
                rs.next();
                System.out.println("Connected to {{.Target.Host}}:{{.Target.Port}} - MySQL " + rs.getString(1));
            }
{{- if .Ops.Schema}}

            try (Statement st = conn.createStatement()) {
                st.executeUpdate("{{scCustomersDDLMySQLOneLine .Table}}");
            }
            System.out.println("Schema ready: {{.Database}}." + TABLE);
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            System.out.println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
            long customerId;
            try (PreparedStatement ps = conn.prepareStatement(
                    "INSERT INTO " + TABLE + " (name, email) VALUES (?, ?)",
                    Statement.RETURN_GENERATED_KEYS)) {
                ps.setString(1, {{scDemoName | q}});
                ps.setString(2, {{scDemoEmail | q}});
                ps.executeUpdate();
                try (ResultSet keys = ps.getGeneratedKeys()) {
                    keys.next();
                    customerId = keys.getLong(1);
                }
            }
            System.out.println("Created customer " + customerId + ": {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

            System.out.println("READ");
            try (PreparedStatement ps = conn.prepareStatement(
                    "SELECT id, name, email FROM " + TABLE + " WHERE id = ?")) {
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
            try (PreparedStatement ps = conn.prepareStatement(
                    "UPDATE " + TABLE + " SET email = ? WHERE id = ?")) {
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
        // try-with-resources closed the connection on the way out, exception or not.
        System.out.println("{{.Scenario.Label}} example completed successfully.");
    }
}
`

const scMySQLHikariJava = `{{.Header "// "}}

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;

public class DbCanvasCrud {

    private static final String URL = "` + scMySQLJDBCURL + `";
    private static final String USER = {{.Target.User | q}};
    private static final String PASSWORD = {{.Target.Password | q}};
    private static final String TABLE = {{.Table | q}};

    // Lab defaults, not a tuning recommendation. A pool of five on a laptop cluster is
    // enough to watch connections being borrowed and returned; sizing a real pool is a
    // question about the server's capacity and the application's concurrency, and neither
    // of those is what this example is showing.
    private static final int MAX_POOL_SIZE = 5;
    private static final int MIN_IDLE = 1;

    private static HikariDataSource pool() {
        HikariConfig cfg = new HikariConfig();
        cfg.setJdbcUrl(URL);
        cfg.setUsername(USER);
        cfg.setPassword(PASSWORD);
        cfg.setMaximumPoolSize(MAX_POOL_SIZE);
        cfg.setMinimumIdle(MIN_IDLE);
        cfg.setPoolName("dbcanvas-sample");
        // Fail fast rather than block forever when the endpoint is unreachable: a lab is
        // where you want the error, not a stalled process.
        cfg.setConnectionTimeout(10_000);
        cfg.setInitializationFailTimeout(10_000);
        return new HikariDataSource(cfg);
    }

    public static void main(String[] args) throws Exception {
        // One pool for the life of the program. Every operation below borrows a connection
        // and returns it by closing it — with a pool, close() means "give it back".
        try (HikariDataSource ds = pool()) {
            try (Connection conn = ds.getConnection();
                 Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("SELECT VERSION()")) {
                rs.next();
                System.out.println("Connected to {{.Target.Host}}:{{.Target.Port}} - MySQL " + rs.getString(1));
                System.out.println("Pool: maximumPoolSize=" + MAX_POOL_SIZE + ", minimumIdle=" + MIN_IDLE);
            }
{{- if .Ops.Schema}}

            try (Connection conn = ds.getConnection(); Statement st = conn.createStatement()) {
                st.executeUpdate("{{scCustomersDDLMySQLOneLine .Table}}");
            }
            System.out.println("Schema ready: {{.Database}}." + TABLE);
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            System.out.println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
            long customerId;
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement(
                     "INSERT INTO " + TABLE + " (name, email) VALUES (?, ?)",
                     Statement.RETURN_GENERATED_KEYS)) {
                ps.setString(1, {{scDemoName | q}});
                ps.setString(2, {{scDemoEmail | q}});
                ps.executeUpdate();
                try (ResultSet keys = ps.getGeneratedKeys()) {
                    keys.next();
                    customerId = keys.getLong(1);
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
        // Leaving the try-with-resources closes the pool, which closes every connection in it.
        System.out.println("{{.Scenario.Label}} example completed successfully.");
    }
}
`

// ------------------------------------------------------------------------------- shell

const scMySQLShell = `#!/usr/bin/env bash
{{.Header "# "}}

set -euo pipefail

HOST={{.Target.Host | sq}}
PORT={{.Target.Port}}
USER={{.Target.User | sq}}
DATABASE={{.Database | sq}}
TABLE={{.Table | sq}}

# MYSQL_PWD keeps the password off the command line, where every other process on the
# node could read it out of ps.
export MYSQL_PWD={{.Target.Password | sq}}

# --ssl-mode is Oracle's and Percona's spelling. MariaDB's client, which is what some
# distributions provide under the name mysql, rejects it outright ("unknown variable 'ssl-mode'") and
# spells the same intent --ssl / --ssl-verify-server-cert. Asking the client which it speaks is
# the only way a script can be right on both.
if mysql --help 2>/dev/null | grep -q -- --ssl-mode; then
{{- if .Verify}}
  # VERIFY_IDENTITY checks the chain and the hostname; the CA is the stack's own, already
  # installed in this node's trust store.
  TLS=(--ssl-mode=VERIFY_IDENTITY --ssl-ca={{.CA | sq}})
{{- else if .Encrypted}}
  TLS=(--ssl-mode=REQUIRED)
{{- else}}
  TLS=(--ssl-mode=DISABLED)
{{- end}}
else
{{- if .Verify}}
  TLS=(--ssl --ssl-verify-server-cert --ssl-ca={{.CA | sq}})
{{- else if .Encrypted}}
  TLS=(--ssl)
{{- else}}
  TLS=(--skip-ssl)
{{- end}}
fi

MYSQL=(mysql --protocol=TCP --host="$HOST" --port="$PORT" --user="$USER" --batch --skip-column-names
  "${TLS[@]}"
{{- if .MTLS}}
  --ssl-cert={{.ClientCert | sq}} --ssl-key={{.ClientKey | sq}}
{{- end}}
)

echo "Connecting to $HOST:$PORT..."
VERSION=$("${MYSQL[@]}" -e 'SELECT VERSION()')
echo "Connected to $HOST:$PORT - MySQL $VERSION"
{{- if .Ops.Schema}}

"${MYSQL[@]}" -e "CREATE DATABASE IF NOT EXISTS $DATABASE"
"${MYSQL[@]}" --database="$DATABASE" <<SQL
{{scCustomersDDLMySQL .Table}};
SQL
echo "Schema ready: $DATABASE.$TABLE"
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

echo "{{if .Ops.Create}}CREATE{{else}}SEED{{end}}"
ID=$("${MYSQL[@]}" --database="$DATABASE" <<SQL
INSERT INTO $TABLE (name, email) VALUES ({{scDemoName | sq}}, {{scDemoEmail | sq}});
SELECT LAST_INSERT_ID();
SQL
)
echo "Created customer $ID: {{scDemoName}}"
{{- end}}
{{- if .Ops.Read}}

echo "READ"
"${MYSQL[@]}" --database="$DATABASE" -e "SELECT id, name, email FROM $TABLE WHERE id = $ID" | tr '\t' '|' | sed 's/|/ | /g'
{{- end}}
{{- if .Ops.Update}}

echo "UPDATE"
"${MYSQL[@]}" --database="$DATABASE" -e "UPDATE $TABLE SET email = {{scDemoNewEmail | sq}} WHERE id = $ID"
echo "Updated customer $ID"
"${MYSQL[@]}" --database="$DATABASE" -e "SELECT id, name, email FROM $TABLE WHERE id = $ID" | tr '\t' '|' | sed 's/|/ | /g'
{{- end}}
{{- if .Ops.Delete}}

echo "DELETE"
"${MYSQL[@]}" --database="$DATABASE" -e "DELETE FROM $TABLE WHERE id = $ID"
echo "Deleted customer $ID"
LEFT=$("${MYSQL[@]}" --database="$DATABASE" -e "SELECT COUNT(*) FROM $TABLE WHERE id = $ID")
echo "Rows with that id now: $LEFT"
{{- end}}

echo "{{.Scenario.Label}} example completed successfully."
`
