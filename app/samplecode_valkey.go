package main

import "strings"

// samplecode_valkey.go — the Valkey samples.
//
// Valkey has no schema and no table, so the shape of the data is a decision the sample has to make
// and explain rather than one the server imposes. The one used here is the ordinary one:
//
//	{dbcanvas}:customer:<id>   a hash holding name, email and created_at
//	{dbcanvas}:customers       a set of the ids that exist, so the keys can be listed without SCAN
//	{dbcanvas}:customer:seq    an integer the ids are allocated from
//
// The braces are not decoration. On a cluster, keys are assigned to slots by hashing the key —
// unless it contains a braced substring, in which case only that substring is hashed. Without it,
// the hash and the index set would land on different shards and the multi-key command that touches
// both would be refused with CROSSSLOT. With it, one sample runs unchanged against a standalone
// node and a cluster, which is the whole point of offering both as endpoints.

var scValkeyClients = []scClient{
	{
		Database: scValkey, Language: "python", ID: "valkey-py", Label: "valkey-py",
		Summary: "The Valkey project's own Python client — a fork of redis-py, so the API is the one most Python code already uses.",
		Runtime: scRuntimePython,
		Deps: []scDep{{
			Manager: "pip", Name: "valkey", Import: "valkey",
			License: "MIT", URL: "https://github.com/valkey-io/valkey-py",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.py", "python", scValkeyPy, g),
				scFileOf("requirements.txt", "text", "valkey\n", g),
			}
		},
		Run: func(g scGen) string { return scVenv + "/bin/python crud.py" },
	},
	{
		Database: scValkey, Language: "node", ID: "iovalkey", Label: "iovalkey",
		Summary: "The Valkey fork of ioredis. Cluster support is the same object with a list of seeds rather than a different client.",
		Runtime: scRuntimeNode,
		Deps: []scDep{{
			Manager: "npm", Name: "iovalkey", Version: "^0.4", License: "MIT",
			URL: "https://github.com/valkey-io/iovalkey",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.js", "javascript", scValkeyJS, g),
				scFileOf("package.json", "json", scNodePackageJSON, g),
			}
		},
		Run: func(g scGen) string { return "node crud.js" },
	},
	{
		Database: scValkey, Language: "go", ID: "go-redis", Label: "go-redis (UniversalClient)",
		Summary: "The RESP client DBCanvas's own Valkey simulators use. UniversalClient picks standalone or cluster mode from the address list, so one program covers both.",
		Runtime: scRuntimeGo,
		Deps: []scDep{{
			Manager: "gomod", Name: "github.com/redis/go-redis/v9", Version: "v9.22.0",
			License: "BSD-2-Clause", URL: "https://github.com/redis/go-redis",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("main.go", "go", scValkeyGo, g),
				scFileOf("go.mod", "text", scGoMod, g),
			}
		},
		Run: func(g scGen) string { return "go run ." },
	},
	{
		Database: scValkey, Language: "java", ID: "valkey-java", Label: "valkey-java",
		Summary: "The Valkey project's Java client, forked from Jedis. JedisPooled for a single node, JedisCluster for a cluster — both the same command surface.",
		Runtime: scRuntimeJava,
		Deps: []scDep{
			{
				Manager: "maven", Name: "io.valkey:valkey-java", Version: "5.5.0",
				License: "MIT", URL: "https://github.com/valkey-io/valkey-java",
			},
			// valkey-java brings slf4j-api 1.7 transitively; a 1.7 API with no binding
			// prints three lines of complaint before the program's own first line. Same
			// pairing as the other Java samples — and slf4j-api is declared explicitly so
			// the 2.x API wins over the transitive 1.7.
			{Manager: "maven", Name: "org.slf4j:slf4j-api", Version: "2.0.19", License: "MIT", URL: "https://www.slf4j.org/"},
			{Manager: "maven", Name: "org.slf4j:slf4j-simple", Version: "2.0.19", License: "MIT", URL: "https://www.slf4j.org/"},
		},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("src/main/java/DbCanvasCrud.java", "java", scValkeyJava, g),
				scFileOf("pom.xml", "xml", scMavenPOM, g),
			}
		},
		Run: func(g scGen) string { return scMavenRun },
	},
	{
		Database: scValkey, Language: "dotnet", ID: "stackexchange-redis", Label: "StackExchange.Redis",
		Summary: "The standard .NET RESP client. One ConnectionMultiplexer serves a standalone node or a cluster — given several seeds it discovers the slot map itself.",
		Runtime: scRuntimeDotnet,
		Deps: []scDep{{
			Manager: "nuget", Name: "StackExchange.Redis", Version: "3.3.1",
			License: "MIT", URL: "https://github.com/StackExchange/StackExchange.Redis",
		}},
		Files: scDotnetFiles(scValkeyCS),
		Run:   func(g scGen) string { return scDotnetRun },
	},
	{
		Database: scValkey, Language: "shell", ID: "valkey-cli", Label: "valkey-cli",
		Summary: "The native client. Every command below is exactly what the drivers above send.",
		Runtime: scRuntimeShell, SysPackages: []string{"valkey-cli"},
		Files: func(g scGen) []scFile {
			return []scFile{scFileOf("crud.sh", "shell", scValkeyShell, g)}
		},
		Run: func(g scGen) string { return "bash crud.sh" },
	},
}

// Cluster reports whether this endpoint is a Valkey cluster, which is the one thing that changes
// the client object in every language here.
func (g scGen) Cluster() bool { return g.Target.Kind == "valkeycluster" && len(g.Target.Hosts) > 1 }

// CustomerKey / CustomersKey / SeqKey are the three keys the samples use, hash tag included.
func (g scGen) CustomerKey() string  { return g.KeyPrefix + "customer:" }
func (g scGen) CustomersKey() string { return g.KeyPrefix + "customers" }
func (g scGen) SeqKey() string       { return g.KeyPrefix + "customer:seq" }

// SeedHosts is the bare hostnames of a multi-host endpoint (no port), for the clients that take
// host and port as two arguments. A single-host endpoint answers with its one host.
func (g scGen) SeedHosts() []string {
	if len(g.Target.Hosts) == 0 {
		return []string{g.Target.Host}
	}
	out := make([]string, 0, len(g.Target.Hosts))
	for _, h := range g.Target.Hosts {
		if i := strings.LastIndex(h, ":"); i > 0 {
			h = h[:i]
		}
		out = append(out, h)
	}
	return out
}

// ValkeyAddrList is every seed address, quoted for a list literal.
func (g scGen) ValkeyAddrList() string {
	var b strings.Builder
	for i, a := range g.Addrs() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"` + a + `"`)
	}
	return b.String()
}

// ------------------------------------------------------------------------------- Python

const scValkeyPy = `{{.Header "# "}}

import datetime

{{if .Cluster}}from valkey.cluster import ValkeyCluster, ClusterNode{{else}}import valkey{{end}}

CUSTOMER_KEY = {{.CustomerKey | q}}
CUSTOMERS_KEY = {{.CustomersKey | q}}
SEQ_KEY = {{.SeqKey | q}}


def connect():
{{- if .Cluster}}
    # A cluster client is seeded with the shards it knows about and discovers the rest,
    # then routes each command to the shard that owns the key.
    return ValkeyCluster(
        startup_nodes=[
{{- range .SeedHosts}}
            ClusterNode({{. | q}}, {{$.Target.Port}}),
{{- end}}
        ],
        password={{.Target.Password | q}},
        decode_responses=True,
        socket_connect_timeout=10,
    )
{{- else}}
    return valkey.Valkey(
        host={{.Target.Host | q}},
        port={{.Target.Port}},
        password={{.Target.Password | q}},
        decode_responses=True,
        socket_connect_timeout=10,
{{- if .Encrypted}}
        ssl=True,
{{- if .Verify}}
        ssl_ca_certs={{.CA | q}},
        ssl_cert_reqs="required",
{{- else}}
        ssl_cert_reqs="none",
{{- end}}
{{- if .MTLS}}
        ssl_certfile={{.ClientCert | q}},
        ssl_keyfile={{.ClientKey | q}},
{{- end}}
{{- end}}
    )
{{- end}}


def main():
    client = connect()
    try:
        info = client.info("server")
        print("Connected to {{.Addr}} - Valkey {}".format(info.get("valkey_version", info.get("redis_version", "?"))))
{{- if .Ops.Schema}}

        # There is no schema to create. What an application does need is somewhere to
        # allocate ids from, and a way to find the customers it has written.
        client.setnx(SEQ_KEY, 0)
        print("Key space ready: {}* with the index at {}".format(CUSTOMER_KEY, CUSTOMERS_KEY))
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

        print("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
        customer_id = client.incr(SEQ_KEY)
        key = CUSTOMER_KEY + str(customer_id)
        client.hset(key, mapping={
            "id": customer_id,
            "name": {{scDemoName | q}},
            "email": {{scDemoEmail | q}},
            "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        })
        client.sadd(CUSTOMERS_KEY, customer_id)
        print("Created customer {}: {}".format(customer_id, {{scDemoName | q}}))
{{- end}}
{{- if .Ops.Read}}

        print("READ")
        doc = client.hgetall(CUSTOMER_KEY + str(customer_id))
        print(" | ".join([doc["id"], doc["name"], doc["email"]]))
{{- end}}
{{- if .Ops.Update}}

        print("UPDATE")
        client.hset(CUSTOMER_KEY + str(customer_id), "email", {{scDemoNewEmail | q}})
        print("Updated customer {}".format(customer_id))
        doc = client.hgetall(CUSTOMER_KEY + str(customer_id))
        print(" | ".join([doc["id"], doc["name"], doc["email"]]))
{{- end}}
{{- if .Ops.Delete}}

        print("DELETE")
        removed = client.delete(CUSTOMER_KEY + str(customer_id))
        client.srem(CUSTOMERS_KEY, customer_id)
        print("Deleted customer {} ({} key)".format(customer_id, removed))
        print("Keys with that id now: {}".format(client.exists(CUSTOMER_KEY + str(customer_id))))
{{- end}}
    finally:
        client.close()
    print("{{.Scenario.Label}} example completed successfully.")


if __name__ == "__main__":
    main()
`

// ------------------------------------------------------------------------------- Node.js

const scValkeyJS = `{{.Header "// "}}

const Valkey = require('iovalkey')
{{- if or .Verify .MTLS}}
const fs = require('node:fs')
{{- end}}

const CUSTOMER_KEY = {{.CustomerKey | q}}
const CUSTOMERS_KEY = {{.CustomersKey | q}}
const SEQ_KEY = {{.SeqKey | q}}

const OPTIONS = {
  password: {{.Target.Password | q}},
  connectTimeout: 10000,
  maxRetriesPerRequest: 2,
{{- if .Encrypted}}
  tls: {
{{- if .Verify}}
    ca: fs.readFileSync({{.CA | q}}),
    rejectUnauthorized: true,
{{- else}}
    rejectUnauthorized: false,
{{- end}}
{{- if .MTLS}}
    cert: fs.readFileSync({{.ClientCert | q}}),
    key: fs.readFileSync({{.ClientKey | q}}),
{{- end}}
  },
{{- end}}
}

{{if .Cluster -}}
// Cluster mode: the seeds are enough, and the client learns the slot map from them.
const client = new Valkey.Cluster(
  [{{range $i, $h := .SeedHosts}}{{if $i}}, {{end}}{ host: {{$h | q}}, port: {{$.Target.Port}} }{{end}}],
  { redisOptions: OPTIONS },
)
{{- else -}}
const client = new Valkey({ host: {{.Target.Host | q}}, port: {{.Target.Port}}, ...OPTIONS })
{{- end}}

async function main() {
  try {
    const info = await client.info('server')
    // Both are reported and redis_version (the compatibility number) comes first, so
    // prefer the Valkey one rather than whichever matches first.
    const version = ((info.match(/^valkey_version:(.*)$/m) || info.match(/^redis_version:(.*)$/m) || [, '?'])[1]).trim()
    console.log(` + "`" + `Connected to {{.Addr}} - Valkey ${version}` + "`" + `)
{{- if .Ops.Schema}}

    // Nothing to create but the counter ids come from, and the set that indexes them.
    await client.setnx(SEQ_KEY, 0)
    console.log(` + "`" + `Key space ready: ${CUSTOMER_KEY}* with the index at ${CUSTOMERS_KEY}` + "`" + `)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

    console.log('{{if .Ops.Create}}CREATE{{else}}SEED{{end}}')
    const customerId = await client.incr(SEQ_KEY)
    await client.hset(CUSTOMER_KEY + customerId, {
      id: customerId,
      name: {{scDemoName | q}},
      email: {{scDemoEmail | q}},
      created_at: new Date().toISOString(),
    })
    await client.sadd(CUSTOMERS_KEY, customerId)
    console.log(` + "`" + `Created customer ${customerId}: {{scDemoName}}` + "`" + `)
{{- end}}
{{- if .Ops.Read}}

    console.log('READ')
    const doc = await client.hgetall(CUSTOMER_KEY + customerId)
    console.log([doc.id, doc.name, doc.email].join(' | '))
{{- end}}
{{- if .Ops.Update}}

    console.log('UPDATE')
    await client.hset(CUSTOMER_KEY + customerId, 'email', {{scDemoNewEmail | q}})
    console.log(` + "`" + `Updated customer ${customerId}` + "`" + `)
    const after = await client.hgetall(CUSTOMER_KEY + customerId)
    console.log([after.id, after.name, after.email].join(' | '))
{{- end}}
{{- if .Ops.Delete}}

    console.log('DELETE')
    const removed = await client.del(CUSTOMER_KEY + customerId)
    await client.srem(CUSTOMERS_KEY, customerId)
    console.log(` + "`" + `Deleted customer ${customerId} (${removed} key)` + "`" + `)
    console.log(` + "`" + `Keys with that id now: ${await client.exists(CUSTOMER_KEY + customerId)}` + "`" + `)
{{- end}}
  } finally {
    await client.quit()
  }
  console.log('{{.Scenario.Label}} example completed successfully.')
}

main().catch((err) => { console.error(err); process.exit(1) })
`

// ------------------------------------------------------------------------------- Go

const scValkeyGo = `{{.Header "// "}}

package main

import (
	"context"
	{{if or .Encrypted .MTLS}}"crypto/tls"
	{{end}}{{if .Verify}}"crypto/x509"
	{{end}}"fmt"
	"log"
	{{if .Verify}}"os"
	{{end}}"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	customerKey  = {{.CustomerKey | q}}
	customersKey = {{.CustomersKey | q}}
	seqKey       = {{.SeqKey | q}}
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &redis.UniversalOptions{
		Addrs:    []string{ {{.ValkeyAddrList}} },
		Password: {{.Target.Password | q}},
	}
{{- if or .Verify .MTLS}}
	tlsCfg := &tls.Config{ServerName: {{.Target.Host | q}}}
{{- if .Verify}}
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
	tlsCfg.InsecureSkipVerify = true
{{- end}}
{{- if .MTLS}}
	clientCert, cerr := tls.LoadX509KeyPair({{.ClientCert | q}}, {{.ClientKey | q}})
	if cerr != nil {
		log.Fatalf("read client certificate: %v", cerr)
	}
	tlsCfg.Certificates = []tls.Certificate{clientCert}
{{- end}}
	opts.TLSConfig = tlsCfg
{{- else if .Encrypted}}
	opts.TLSConfig = &tls.Config{InsecureSkipVerify: true}
{{- end}}

	// UniversalClient reads the address list: several addresses means cluster mode,
	// one means a plain client. The commands below are identical either way.
	client := redis.NewUniversalClient(opts)
	defer client.Close()

	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	version := "?"
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "valkey_version:"); ok {
			version = v
		} else if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok && version == "?" {
			version = v
		}
	}
	fmt.Printf("Connected to {{.Addr}} - Valkey %s\n", version)
{{- if .Ops.Schema}}

	if err := client.SetNX(ctx, seqKey, 0, 0).Err(); err != nil {
		log.Fatalf("prepare the id counter: %v", err)
	}
	fmt.Printf("Key space ready: %s* with the index at %s\n", customerKey, customersKey)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

	fmt.Println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
	customerID, err := client.Incr(ctx, seqKey).Result()
	if err != nil {
		log.Fatalf("allocate an id: %v", err)
	}
	key := fmt.Sprintf("%s%d", customerKey, customerID)
	if err := client.HSet(ctx, key,
		"id", customerID,
		"name", {{scDemoName | q}},
		"email", {{scDemoEmail | q}},
		"created_at", time.Now().UTC().Format(time.RFC3339),
	).Err(); err != nil {
		log.Fatalf("write the customer: %v", err)
	}
	if err := client.SAdd(ctx, customersKey, customerID).Err(); err != nil {
		log.Fatalf("index the customer: %v", err)
	}
	fmt.Printf("Created customer %d: %s\n", customerID, {{scDemoName | q}})
{{- end}}
{{- if .Ops.Read}}

	fmt.Println("READ")
	doc, err := client.HGetAll(ctx, key).Result()
	if err != nil {
		log.Fatalf("read: %v", err)
	}
	fmt.Printf("%s | %s | %s\n", doc["id"], doc["name"], doc["email"])
{{- end}}
{{- if .Ops.Update}}

	fmt.Println("UPDATE")
	if err := client.HSet(ctx, key, "email", {{scDemoNewEmail | q}}).Err(); err != nil {
		log.Fatalf("update: %v", err)
	}
	fmt.Printf("Updated customer %d\n", customerID)
	after, err := client.HGetAll(ctx, key).Result()
	if err != nil {
		log.Fatalf("re-read: %v", err)
	}
	fmt.Printf("%s | %s | %s\n", after["id"], after["name"], after["email"])
{{- end}}
{{- if .Ops.Delete}}

	fmt.Println("DELETE")
	removed, err := client.Del(ctx, key).Result()
	if err != nil {
		log.Fatalf("delete: %v", err)
	}
	if err := client.SRem(ctx, customersKey, customerID).Err(); err != nil {
		log.Fatalf("de-index: %v", err)
	}
	fmt.Printf("Deleted customer %d (%d key)\n", customerID, removed)
	left, err := client.Exists(ctx, key).Result()
	if err != nil {
		log.Fatalf("exists: %v", err)
	}
	fmt.Printf("Keys with that id now: %d\n", left)
{{- end}}

	fmt.Println("{{.Scenario.Label}} example completed successfully.")
}
`

// ------------------------------------------------------------------------------- Java

const scValkeyJava = `{{.Header "// "}}

import io.valkey.DefaultJedisClientConfig;
import io.valkey.HostAndPort;
{{if .Cluster}}import io.valkey.JedisCluster;{{else}}import io.valkey.JedisPooled;{{end}}
import io.valkey.Protocol;
import io.valkey.UnifiedJedis;

import java.nio.charset.StandardCharsets;
import java.time.Instant;
{{- if .Cluster}}
import java.util.HashSet;
import java.util.Set;
{{- end}}
import java.util.LinkedHashMap;
import java.util.Map;

public class DbCanvasCrud {

    private static final String CUSTOMER_KEY = {{.CustomerKey | q}};
    private static final String CUSTOMERS_KEY = {{.CustomersKey | q}};
    private static final String SEQ_KEY = {{.SeqKey | q}};

    private static UnifiedJedis connect() {
        DefaultJedisClientConfig config = DefaultJedisClientConfig.builder()
                .password({{.Target.Password | q}})
                .connectionTimeoutMillis(10_000)
                .ssl({{if .Encrypted}}true{{else}}false{{end}})
                .build();
{{- if .Cluster}}
        Set<HostAndPort> seeds = new HashSet<>();
{{- range .Addrs}}
        seeds.add(HostAndPort.from({{. | q}}));
{{- end}}
        return new JedisCluster(seeds, config);
{{- else}}
        return new JedisPooled(new HostAndPort({{.Target.Host | q}}, {{.Target.Port}}), config);
{{- end}}
    }

    public static void main(String[] args) {
{{- if .Encrypted}}
        // The JVM takes trust material from a keystore; DBCanvas built one from the stack
        // CA with keytool before this ran.
        System.setProperty("javax.net.ssl.trustStore", {{.Truststore | q}});
        System.setProperty("javax.net.ssl.trustStoreType", "PKCS12");
        System.setProperty("javax.net.ssl.trustStorePassword", {{.StorePass | q}});
{{- end}}
        try (UnifiedJedis client = connect()) {
            // UnifiedJedis wraps the data commands; INFO is a server command, so it goes
            // through sendCommand — which is also how you send anything the client has no
            // method for.
            String info = new String((byte[]) client.sendCommand(Protocol.Command.INFO, "server"),
                    StandardCharsets.UTF_8);
            String version = "?";
            for (String line : info.split("\r?\n")) {
                if (line.startsWith("valkey_version:") || (version.equals("?") && line.startsWith("redis_version:"))) {
                    version = line.substring(line.indexOf(':') + 1).trim();
                }
            }
            System.out.println("Connected to {{.Addr}} - Valkey " + version);
{{- if .Ops.Schema}}

            client.setnx(SEQ_KEY, "0");
            System.out.println("Key space ready: " + CUSTOMER_KEY + "* with the index at " + CUSTOMERS_KEY);
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            System.out.println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
            long customerId = client.incr(SEQ_KEY);
            String key = CUSTOMER_KEY + customerId;
            Map<String, String> customer = new LinkedHashMap<>();
            customer.put("id", String.valueOf(customerId));
            customer.put("name", {{scDemoName | q}});
            customer.put("email", {{scDemoEmail | q}});
            customer.put("created_at", Instant.now().toString());
            client.hset(key, customer);
            client.sadd(CUSTOMERS_KEY, String.valueOf(customerId));
            System.out.println("Created customer " + customerId + ": {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

            System.out.println("READ");
            Map<String, String> doc = client.hgetAll(key);
            System.out.println(doc.get("id") + " | " + doc.get("name") + " | " + doc.get("email"));
{{- end}}
{{- if .Ops.Update}}

            System.out.println("UPDATE");
            client.hset(key, "email", {{scDemoNewEmail | q}});
            System.out.println("Updated customer " + customerId);
            Map<String, String> after = client.hgetAll(key);
            System.out.println(after.get("id") + " | " + after.get("name") + " | " + after.get("email"));
{{- end}}
{{- if .Ops.Delete}}

            System.out.println("DELETE");
            long removed = client.del(key);
            client.srem(CUSTOMERS_KEY, String.valueOf(customerId));
            System.out.println("Deleted customer " + customerId + " (" + removed + " key)");
            // exists(String) answers a boolean here, where every other client in this
            // registry answers a count. Printed as a count so the six languages' output
            // can be compared line for line.
            System.out.println("Keys with that id now: " + (client.exists(key) ? 1 : 0));
{{- end}}
        }
        System.out.println("{{.Scenario.Label}} example completed successfully.");
    }
}
`

// ------------------------------------------------------------------------------- shell

const scValkeyShell = `#!/usr/bin/env bash
{{.Header "# "}}

set -euo pipefail

CUSTOMER_KEY={{.CustomerKey | sq}}
CUSTOMERS_KEY={{.CustomersKey | sq}}
SEQ_KEY={{.SeqKey | sq}}

# REDISCLI_AUTH is read by valkey-cli, which keeps the password out of ps and out of
# the "-a used on the command line" warning.
export REDISCLI_AUTH={{.Target.Password | sq}}

CLI=(valkey-cli -h {{.Target.Host | sq}} -p {{.Target.Port}} --no-auth-warning
{{- if .Cluster}}
  # -c follows the MOVED redirections a cluster answers with.
  -c
{{- end}}
{{- if .Encrypted}}
  --tls
{{- if .Verify}}
  --cacert {{.CA | sq}}
{{- end}}
{{- if .MTLS}}
  --cert {{.ClientCert | sq}} --key {{.ClientKey | sq}}
{{- end}}
{{- end}}
)

echo "Connecting to {{.Addr}}..."
# A Valkey server reports both valkey_version and redis_version (the compatibility
# number, still 7.2.x), and redis_version comes first — so taking whichever matches
# first answers with the wrong one.
INFO=$("${CLI[@]}" INFO server)
VERSION=$(printf '%s' "$INFO" | sed -n 's/^valkey_version:\(.*\)$/\1/p' | head -1 | tr -d '\r')
[ -n "$VERSION" ] || VERSION=$(printf '%s' "$INFO" | sed -n 's/^redis_version:\(.*\)$/\1/p' | head -1 | tr -d '\r')
echo "Connected to {{.Addr}} - Valkey $VERSION"
{{- if .Ops.Schema}}

"${CLI[@]}" SETNX "$SEQ_KEY" 0 >/dev/null
echo "Key space ready: $CUSTOMER_KEY* with the index at $CUSTOMERS_KEY"
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

echo "{{if .Ops.Create}}CREATE{{else}}SEED{{end}}"
ID=$("${CLI[@]}" INCR "$SEQ_KEY")
KEY="$CUSTOMER_KEY$ID"
"${CLI[@]}" HSET "$KEY" id "$ID" name {{scDemoName | sq}} email {{scDemoEmail | sq}} created_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
"${CLI[@]}" SADD "$CUSTOMERS_KEY" "$ID" >/dev/null
echo "Created customer $ID: {{scDemoName}}"
{{- end}}
{{- if .Ops.Read}}

echo "READ"
echo "$("${CLI[@]}" HGET "$KEY" id) | $("${CLI[@]}" HGET "$KEY" name) | $("${CLI[@]}" HGET "$KEY" email)"
{{- end}}
{{- if .Ops.Update}}

echo "UPDATE"
"${CLI[@]}" HSET "$KEY" email {{scDemoNewEmail | sq}} >/dev/null
echo "Updated customer $ID"
echo "$("${CLI[@]}" HGET "$KEY" id) | $("${CLI[@]}" HGET "$KEY" name) | $("${CLI[@]}" HGET "$KEY" email)"
{{- end}}
{{- if .Ops.Delete}}

echo "DELETE"
REMOVED=$("${CLI[@]}" DEL "$KEY")
"${CLI[@]}" SREM "$CUSTOMERS_KEY" "$ID" >/dev/null
echo "Deleted customer $ID ($REMOVED key)"
echo "Keys with that id now: $("${CLI[@]}" EXISTS "$KEY")"
{{- end}}

echo "{{.Scenario.Label}} example completed successfully."
`

// ------------------------------------------------------------------------------- C#

const scValkeyCS = `{{.Header "// "}}

using StackExchange.Redis;
{{- if .MTLS}}
using System.Security.Cryptography.X509Certificates;
{{- end}}
{{- if .Ops.Any}}

const string CustomerKey = {{.CustomerKey | cs}};
const string CustomersKey = {{.CustomersKey | cs}};
const string SeqKey = {{.SeqKey | cs}};
{{- end}}

var options = new ConfigurationOptions
{
    Password = {{.Target.Password | cs}},
    ConnectTimeout = 10000,
    // Fail on the first attempt rather than retrying in the background: a lab wants
    // the error, not a program that waits for a server that is not coming.
    AbortOnConnectFail = true,
    // INFO is on the client's list of admin commands, which it refuses to send unless
    // asked to. The program sends nothing else of the kind.
    AllowAdmin = true,
{{- if .Encrypted}}
    Ssl = true,
{{- end}}
};
{{- if .Cluster}}
// Cluster mode: the seeds are enough, and the multiplexer learns the slot map from them.
{{- end}}
{{- range .Addrs}}
options.EndPoints.Add({{. | cs}});
{{- end}}
{{- if .Verify}}
// TrustIssuer accepts a chain that ends at this CA and nothing else; the host name is
// still checked, per endpoint.
options.TrustIssuer({{.CA | cs}});
{{- else if .Encrypted}}
// Encrypted, but the server is not identified.
options.CertificateValidation += (_, _, _, _) => true;
{{- end}}
{{- if .MTLS}}
// Mutual TLS: the certificate this client presents, issued by the stack CA.
var clientCertificate = X509Certificate2.CreateFromPemFile({{.ClientCert | cs}}, {{.ClientKey | cs}});
options.CertificateSelection += (_, _, _, _, _) => clientCertificate;
{{- end}}

await using var mux = await ConnectionMultiplexer.ConnectAsync(options);
var db = mux.GetDatabase();

// Both are reported and redis_version (the compatibility number) comes first, so
// prefer the Valkey one rather than whichever matches first.
var info = await mux.GetServer(mux.GetEndPoints()[0]).InfoRawAsync("server") ?? "";
string? Field(string name) => info.Split('\n').Select(l => l.Trim())
    .FirstOrDefault(l => l.StartsWith(name + ":"))?[(name.Length + 1)..];
Console.WriteLine($"Connected to {{.Addr}} - Valkey {Field("valkey_version") ?? Field("redis_version") ?? "?"}");
{{- if .Ops.Schema}}

// Nothing to create but the counter ids come from, and the set that indexes them.
await db.StringSetAsync(SeqKey, 0, when: When.NotExists);
Console.WriteLine($"Key space ready: {CustomerKey}* with the index at {CustomersKey}");
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

Console.WriteLine("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
var customerId = await db.StringIncrementAsync(SeqKey);
var key = CustomerKey + customerId;
await db.HashSetAsync(key, new HashEntry[]
{
    new("id", customerId),
    new("name", {{scDemoName | cs}}),
    new("email", {{scDemoEmail | cs}}),
    new("created_at", DateTime.UtcNow.ToString("O")),
});
await db.SetAddAsync(CustomersKey, customerId);
Console.WriteLine($"Created customer {customerId}: {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

Console.WriteLine("READ");
var doc = (await db.HashGetAllAsync(key)).ToStringDictionary();
Console.WriteLine($"{doc["id"]} | {doc["name"]} | {doc["email"]}");
{{- end}}
{{- if .Ops.Update}}

Console.WriteLine("UPDATE");
await db.HashSetAsync(key, "email", {{scDemoNewEmail | cs}});
Console.WriteLine($"Updated customer {customerId}");
var after = (await db.HashGetAllAsync(key)).ToStringDictionary();
Console.WriteLine($"{after["id"]} | {after["name"]} | {after["email"]}");
{{- end}}
{{- if .Ops.Delete}}

Console.WriteLine("DELETE");
var removed = await db.KeyDeleteAsync(key);
await db.SetRemoveAsync(CustomersKey, customerId);
Console.WriteLine($"Deleted customer {customerId} ({(removed ? 1 : 0)} key)");
Console.WriteLine($"Keys with that id now: {(await db.KeyExistsAsync(key) ? 1 : 0)}");
{{- end}}

Console.WriteLine("{{.Scenario.Label}} example completed successfully.");
`
