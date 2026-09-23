package main

import (
	"fmt"
	"net/url"
	"strings"
)

// samplecode_mongodb.go — the MongoDB samples.
//
// MongoDB is the family where the *endpoint* does most of the work, which is why the URI is built
// once here and every driver is handed the same one. A standalone node, a replica set and a
// sharded cluster are three different connection strings — directConnection for the first,
// replicaSet for the second, a mongos host list for the third — and getting that wrong produces
// the classic confusion where reads work and writes fail, or where a failover is never noticed.
// The target resolver already knows which shape it found (samplecode_targets.go), so the sample
// does not have to guess.

var scMongoClients = []scClient{
	{
		Database: scMongoDB, Language: "python", ID: "pymongo", Label: "PyMongo",
		Summary: "MongoDB's own Python driver. The samples use explicit documents and filters rather than an ODM, so what goes on the wire is visible.",
		Runtime: scRuntimePython,
		Deps: []scDep{{
			Manager: "pip", Name: "pymongo", Import: "pymongo",
			License: "Apache-2.0", URL: "https://github.com/mongodb/mongo-python-driver",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.py", "python", scPyMongoPy, g),
				scFileOf("requirements.txt", "text", "pymongo\n", g),
			}
		},
		Run: func(g scGen) string { return scVenv + "/bin/python crud.py" },
	},
	{
		Database: scMongoDB, Language: "node", ID: "mongodb", Label: "mongodb (Node.js driver)",
		Summary: "The official Node.js driver, with its promise API and the connection closed in a finally block.",
		Runtime: scRuntimeNode,
		Deps: []scDep{{
			Manager: "npm", Name: "mongodb", Version: "^6", License: "Apache-2.0",
			URL: "https://github.com/mongodb/node-mongodb-native",
			// 6 and not 7 because of the Node the base images ship: driver 7 declares
			// engines.node >= 20.19, npm installs it against Oracle Linux 9's Node 16
			// anyway (engines is advisory), and the first connection dies in the driver
			// with "crypto is not defined". Driver 6 supports Node >= 16.20.1, which every
			// DBCanvas base image satisfies. Found by running the sample.
			Note: "Pinned to the 6.x line: driver 7 requires Node 20.19+, which is newer than the Node some DBCanvas base images package.",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.js", "javascript", scMongoJS, g),
				scFileOf("package.json", "json", scNodePackageJSON, g),
			}
		},
		Run: func(g scGen) string { return "node crud.js" },
	},
	{
		Database: scMongoDB, Language: "go", ID: "mongo-driver", Label: "mongo-driver",
		Summary: "The official Go driver, with bson.D filters and a context on every call — which is how a Go service actually bounds a database operation.",
		Runtime: scRuntimeGo,
		Deps: []scDep{{
			Manager: "gomod", Name: "go.mongodb.org/mongo-driver", Version: "v1.17.9",
			License: "Apache-2.0", URL: "https://github.com/mongodb/mongo-go-driver",
		}},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("main.go", "go", scMongoGo, g),
				scFileOf("go.mod", "text", scGoMod, g),
			}
		},
		Run: func(g scGen) string { return "go run ." },
	},
	{
		Database: scMongoDB, Language: "java", ID: "mongodb-driver", Label: "MongoDB Java Driver (sync)",
		Summary: "The synchronous Java driver. TLS material comes from a JVM truststore rather than a PEM path, which is the one thing that always differs from the other languages.",
		Runtime: scRuntimeJava,
		Deps: []scDep{
			{
				Manager: "maven", Name: "org.mongodb:mongodb-driver-sync", Version: "5.5.1",
				License: "Apache-2.0", URL: "https://github.com/mongodb/mongo-java-driver",
			},
			// The driver looks for an SLF4J binding at startup and says so on stderr when
			// it finds none, which is noise on an otherwise clean run. Same pairing and
			// same reasoning as the HikariCP samples: slf4j-simple is MIT, Logback is
			// EPL-1.0 and GPL-incompatible.
			{Manager: "maven", Name: "org.slf4j:slf4j-api", Version: "2.0.19", License: "MIT", URL: "https://www.slf4j.org/"},
			{Manager: "maven", Name: "org.slf4j:slf4j-simple", Version: "2.0.19", License: "MIT", URL: "https://www.slf4j.org/"},
		},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("src/main/java/DbCanvasCrud.java", "java", scMongoJava, g),
				scFileOf("pom.xml", "xml", scMavenPOM, g),
			}
		},
		Run: func(g scGen) string { return scMavenRun },
	},
	{
		Database: scMongoDB, Language: "dotnet", ID: "mongodb-driver", Label: "MongoDB C# Driver",
		Summary: "The official .NET driver, with BsonDocument filters built through Builders<T> rather than a mapped class, so the documents on the wire are the ones in the code.",
		Runtime: scRuntimeDotnet,
		Deps: []scDep{{
			Manager: "nuget", Name: "MongoDB.Driver", Version: "3.12.0",
			License: "Apache-2.0", URL: "https://github.com/mongodb/mongo-csharp-driver",
		}},
		Files: scDotnetFiles(scMongoCS),
		Run:   func(g scGen) string { return scDotnetRun },
	},
	{
		Database: scMongoDB, Language: "shell", ID: "mongosh", Label: "mongosh",
		Summary: "The MongoDB Shell, driven from a script. mongosh is a JavaScript runtime, so the sample is the same program the drivers run, one layer closer to the server.",
		Runtime: scRuntimeShell, SysPackages: []string{"mongosh"},
		Files: func(g scGen) []scFile {
			return []scFile{
				scFileOf("crud.js", "javascript", scMongoshJS, g),
				scFileOf("crud.sh", "shell", scMongoshShell, g),
			}
		},
		Run: func(g scGen) string { return "bash crud.sh" },
	},
}

// MongoHosts is every address of the endpoint, comma separated, which is the host list a MongoDB
// URI takes.
func (g scGen) MongoHosts() string { return strings.Join(g.Addrs(), ",") }

// MongoParams is the query string for this endpoint's shape, TLS excluded (every driver wants that
// expressed differently, and two of them want it outside the URI entirely).
//
// directConnection matters more than it looks: without it a driver given one host of a replica set
// will still go looking for the set's primary, which is right for an application and wrong for
// "connect to this node and tell me what *this node* thinks".
func (g scGen) MongoParams() string {
	params := []string{"authSource=" + g.Target.AuthDB}
	switch {
	case g.Target.ReplicaSet != "" && len(g.Target.Hosts) > 1:
		params = append(params, "replicaSet="+g.Target.ReplicaSet)
	case g.Target.Role != "router" && len(g.Target.Hosts) <= 1:
		params = append(params, "directConnection=true")
	}
	return strings.Join(params, "&")
}

// MongoURI is the connection string without TLS options: credentials, hosts, and the shape.
func (g scGen) MongoURI() string {
	return fmt.Sprintf("mongodb://%s:%s@%s/?%s",
		url.QueryEscape(g.Target.User), url.QueryEscape(g.Target.Password),
		g.MongoHosts(), g.MongoParams())
}

// MongoURITLS is the same URI with the TLS options the drivers that read them from the string
// (the Go driver and mongosh's --tls flags aside, the Node driver too) expect.
func (g scGen) MongoURITLS() string {
	uri := g.MongoURI()
	switch {
	case g.Verify():
		uri += "&tls=true&tlsCAFile=" + url.QueryEscape(g.CA)
	case g.Encrypted():
		uri += "&tls=true&tlsAllowInvalidCertificates=true"
	}
	if g.MTLS() {
		uri += "&tlsCertificateKeyFile=" + url.QueryEscape(g.ClientPEM())
	}
	return uri
}

// ------------------------------------------------------------------------------- Python

const scPyMongoPy = `{{.Header "# "}}

import datetime

from pymongo import MongoClient

URI = {{.MongoURI | q}}
DATABASE = {{.Database | q}}
COLLECTION = {{.Collection | q}}

OPTIONS = {
    "serverSelectionTimeoutMS": 10000,
{{- if .Verify}}
    # The server's certificate is signed by the stack CA, already in this node's trust store.
    "tls": True,
    "tlsCAFile": {{.CA | q}},
{{- else if .Encrypted}}
    "tls": True,
    "tlsAllowInvalidCertificates": True,
{{- end}}
{{- if .MTLS}}
    # PyMongo wants the certificate and its key in one PEM file; DBCanvas wrote both
    # halves and this is the concatenation of them.
    "tlsCertificateKeyFile": {{.ClientPEM | q}},
{{- end}}
}


def main():
    client = MongoClient(URI, **OPTIONS)
    try:
        info = client.admin.command("hello")
        build = client.admin.command("buildInfo")
        print("Connected to {} - MongoDB {}".format(info.get("me", {{.Target.Host | q}}), build["version"]))
{{- if .Ops.Schema}}

        db = client[DATABASE]
        customers = db[COLLECTION]
        # A collection appears on first write, so "schema" here is the index an
        # application would actually want: one customer per email address.
        customers.create_index("email", unique=True)
        print("Collection ready: {}.{}".format(DATABASE, COLLECTION))
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

        print("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
        customers.replace_one(
            {"email": {{scDemoEmail | q}}},
            {
                "_id": 1,
                "name": {{scDemoName | q}},
                "email": {{scDemoEmail | q}},
                "created_at": datetime.datetime.now(datetime.timezone.utc),
            },
            upsert=True,
        )
        customer_id = 1
        print("Created customer {}: {}".format(customer_id, {{scDemoName | q}}))
{{- end}}
{{- if .Ops.Read}}

        print("READ")
        for doc in customers.find({"_id": customer_id}):
            print(" | ".join([str(doc["_id"]), doc["name"], doc["email"]]))
{{- end}}
{{- if .Ops.Update}}

        print("UPDATE")
        res = customers.update_one({"_id": customer_id}, {"$set": {"email": {{scDemoNewEmail | q}}}})
        print("Updated customer {} ({} document)".format(customer_id, res.modified_count))
        doc = customers.find_one({"_id": customer_id})
        print(" | ".join([str(doc["_id"]), doc["name"], doc["email"]]))
{{- end}}
{{- if .Ops.Delete}}

        print("DELETE")
        res = customers.delete_one({"_id": customer_id})
        print("Deleted customer {} ({} document)".format(customer_id, res.deleted_count))
        print("Documents with that id now: {}".format(customers.count_documents({"_id": customer_id})))
{{- end}}
    finally:
        client.close()
    print("{{.Scenario.Label}} example completed successfully.")


if __name__ == "__main__":
    main()
`

// ------------------------------------------------------------------------------- Node.js

const scMongoJS = `{{.Header "// "}}

const { MongoClient } = require('mongodb')

const URI = {{.MongoURI | q}}
const DATABASE = {{.Database | q}}
const COLLECTION = {{.Collection | q}}

const OPTIONS = {
  serverSelectionTimeoutMS: 10000,
{{- if .Verify}}
  tls: true,
  tlsCAFile: {{.CA | q}},
{{- else if .Encrypted}}
  tls: true,
  tlsAllowInvalidCertificates: true,
{{- end}}
{{- if .MTLS}}
  tlsCertificateKeyFile: {{.ClientPEM | q}},
{{- end}}
}

async function main() {
  const client = new MongoClient(URI, OPTIONS)
  await client.connect()
  try {
    const build = await client.db('admin').command({ buildInfo: 1 })
    console.log(` + "`" + `Connected to {{.Target.Host}}:{{.Target.Port}} - MongoDB ${build.version}` + "`" + `)
{{- if .Ops.Schema}}

    const customers = client.db(DATABASE).collection(COLLECTION)
    await customers.createIndex({ email: 1 }, { unique: true })
    console.log(` + "`" + `Collection ready: ${DATABASE}.${COLLECTION}` + "`" + `)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

    console.log('{{if .Ops.Create}}CREATE{{else}}SEED{{end}}')
    const customerId = 1
    await customers.replaceOne(
      { _id: customerId },
      { name: {{scDemoName | q}}, email: {{scDemoEmail | q}}, created_at: new Date() },
      { upsert: true },
    )
    console.log(` + "`" + `Created customer ${customerId}: {{scDemoName}}` + "`" + `)
{{- end}}
{{- if .Ops.Read}}

    console.log('READ')
    for await (const doc of customers.find({ _id: customerId })) {
      console.log([doc._id, doc.name, doc.email].join(' | '))
    }
{{- end}}
{{- if .Ops.Update}}

    console.log('UPDATE')
    const upd = await customers.updateOne({ _id: customerId }, { $set: { email: {{scDemoNewEmail | q}} } })
    console.log(` + "`" + `Updated customer ${customerId} (${upd.modifiedCount} document)` + "`" + `)
    const after = await customers.findOne({ _id: customerId })
    console.log([after._id, after.name, after.email].join(' | '))
{{- end}}
{{- if .Ops.Delete}}

    console.log('DELETE')
    const del = await customers.deleteOne({ _id: customerId })
    console.log(` + "`" + `Deleted customer ${customerId} (${del.deletedCount} document)` + "`" + `)
    console.log(` + "`" + `Documents with that id now: ${await customers.countDocuments({ _id: customerId })}` + "`" + `)
{{- end}}
  } finally {
    await client.close()
  }
  console.log('{{.Scenario.Label}} example completed successfully.')
}

main().catch((err) => { console.error(err); process.exit(1) })
`

// ------------------------------------------------------------------------------- Go

const scMongoGo = `{{.Header "// "}}

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	uri        = {{.MongoURITLS | q}}
	database   = {{.Database | q}}
	collection = {{.Collection | q}}
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The driver reads tls, tlsCAFile and tlsCertificateKeyFile out of the URI itself, so
	// nothing here has to build a tls.Config by hand.
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := client.Disconnect(context.Background()); err != nil {
			log.Printf("disconnect: %v", err)
		}
	}()

	var build bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{"{"}}{Key: "buildInfo", Value: 1}}).Decode(&build); err != nil {
		log.Fatalf("buildInfo: %v", err)
	}
	fmt.Printf("Connected to {{.Target.Host}}:{{.Target.Port}} - MongoDB %v\n", build["version"])
{{- if .Ops.Schema}}

	customers := client.Database(database).Collection(collection)
	if _, err := customers.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"{"}}{Key: "email", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		log.Fatalf("create index: %v", err)
	}
	fmt.Printf("Collection ready: %s.%s\n", database, collection)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

	fmt.Println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}")
	customerID := 1
	if _, err := customers.ReplaceOne(ctx,
		bson.D{{"{"}}{Key: "_id", Value: customerID}},
		bson.D{
			{Key: "name", Value: {{scDemoName | q}}},
			{Key: "email", Value: {{scDemoEmail | q}}},
			{Key: "created_at", Value: time.Now().UTC()},
		},
		options.Replace().SetUpsert(true),
	); err != nil {
		log.Fatalf("insert: %v", err)
	}
	fmt.Printf("Created customer %d: %s\n", customerID, {{scDemoName | q}})
{{- end}}
{{- if .Ops.Read}}

	fmt.Println("READ")
	cur, err := customers.Find(ctx, bson.D{{"{"}}{Key: "_id", Value: customerID}})
	if err != nil {
		log.Fatalf("find: %v", err)
	}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			log.Fatalf("decode: %v", err)
		}
		fmt.Printf("%v | %v | %v\n", doc["_id"], doc["name"], doc["email"])
	}
	cur.Close(ctx)
{{- end}}
{{- if .Ops.Update}}

	fmt.Println("UPDATE")
	upd, err := customers.UpdateOne(ctx,
		bson.D{{"{"}}{Key: "_id", Value: customerID}},
		bson.D{{"{"}}{Key: "$set", Value: bson.D{{"{"}}{Key: "email", Value: {{scDemoNewEmail | q}}}}}},
	)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	fmt.Printf("Updated customer %d (%d document)\n", customerID, upd.ModifiedCount)
	var after bson.M
	if err := customers.FindOne(ctx, bson.D{{"{"}}{Key: "_id", Value: customerID}}).Decode(&after); err != nil {
		log.Fatalf("re-read: %v", err)
	}
	fmt.Printf("%v | %v | %v\n", after["_id"], after["name"], after["email"])
{{- end}}
{{- if .Ops.Delete}}

	fmt.Println("DELETE")
	del, err := customers.DeleteOne(ctx, bson.D{{"{"}}{Key: "_id", Value: customerID}})
	if err != nil {
		log.Fatalf("delete: %v", err)
	}
	fmt.Printf("Deleted customer %d (%d document)\n", customerID, del.DeletedCount)
	left, err := customers.CountDocuments(ctx, bson.D{{"{"}}{Key: "_id", Value: customerID}})
	if err != nil {
		log.Fatalf("count: %v", err)
	}
	fmt.Printf("Documents with that id now: %d\n", left)
{{- end}}

	fmt.Println("{{.Scenario.Label}} example completed successfully.")
}
`

// ------------------------------------------------------------------------------- Java

const scMongoJava = `{{.Header "// "}}

import com.mongodb.ConnectionString;
import com.mongodb.MongoClientSettings;
import com.mongodb.client.MongoClient;
import com.mongodb.client.MongoClients;
import com.mongodb.client.MongoCollection;
import com.mongodb.client.model.Filters;
import com.mongodb.client.model.IndexOptions;
import com.mongodb.client.model.ReplaceOptions;
import com.mongodb.client.model.Updates;
import org.bson.Document;

import java.time.Instant;
import java.util.Date;

public class DbCanvasCrud {

    private static final String URI = {{.MongoURI | q}};
    private static final String DATABASE = {{.Database | q}};
    private static final String COLLECTION = {{.Collection | q}};

    public static void main(String[] args) {
{{- if .Encrypted}}
        // The JVM reads trust material from a keystore, never from a PEM file — so
        // DBCanvas converted the stack CA into truststore.p12 with keytool before this
        // ran, and these properties are what point the driver's SSLContext at it.
        System.setProperty("javax.net.ssl.trustStore", {{.Truststore | q}});
        System.setProperty("javax.net.ssl.trustStoreType", "PKCS12");
        System.setProperty("javax.net.ssl.trustStorePassword", {{.StorePass | q}});
{{- if .MTLS}}
        System.setProperty("javax.net.ssl.keyStore", {{.Keystore | q}});
        System.setProperty("javax.net.ssl.keyStoreType", "PKCS12");
        System.setProperty("javax.net.ssl.keyStorePassword", {{.StorePass | q}});
{{- end}}
{{- end}}
        MongoClientSettings settings = MongoClientSettings.builder()
                .applyConnectionString(new ConnectionString(URI))
{{- if .Encrypted}}
                .applyToSslSettings(b -> b.enabled(true){{if not .Verify}}.invalidHostNameAllowed(true){{end}})
{{- end}}
                .build();

        try (MongoClient client = MongoClients.create(settings)) {
            Document build = client.getDatabase("admin").runCommand(new Document("buildInfo", 1));
            System.out.println("Connected to {{.Target.Host}}:{{.Target.Port}} - MongoDB " + build.getString("version"));
{{- if .Ops.Schema}}

            MongoCollection<Document> customers = client.getDatabase(DATABASE).getCollection(COLLECTION);
            customers.createIndex(new Document("email", 1), new IndexOptions().unique(true));
            System.out.println("Collection ready: " + DATABASE + "." + COLLECTION);
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

            System.out.println("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
            int customerId = 1;
            customers.replaceOne(
                    Filters.eq("_id", customerId),
                    new Document("name", {{scDemoName | q}})
                            .append("email", {{scDemoEmail | q}})
                            .append("created_at", Date.from(Instant.now())),
                    new ReplaceOptions().upsert(true));
            System.out.println("Created customer " + customerId + ": {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

            System.out.println("READ");
            for (Document doc : customers.find(Filters.eq("_id", customerId))) {
                System.out.println(doc.get("_id") + " | " + doc.getString("name") + " | " + doc.getString("email"));
            }
{{- end}}
{{- if .Ops.Update}}

            System.out.println("UPDATE");
            long changed = customers.updateOne(Filters.eq("_id", customerId),
                    Updates.set("email", {{scDemoNewEmail | q}})).getModifiedCount();
            System.out.println("Updated customer " + customerId + " (" + changed + " document)");
            Document after = customers.find(Filters.eq("_id", customerId)).first();
            System.out.println(after.get("_id") + " | " + after.getString("name") + " | " + after.getString("email"));
{{- end}}
{{- if .Ops.Delete}}

            System.out.println("DELETE");
            long removed = customers.deleteOne(Filters.eq("_id", customerId)).getDeletedCount();
            System.out.println("Deleted customer " + customerId + " (" + removed + " document)");
            System.out.println("Documents with that id now: " + customers.countDocuments(Filters.eq("_id", customerId)));
{{- end}}
        }
        System.out.println("{{.Scenario.Label}} example completed successfully.");
    }
}
`

// ------------------------------------------------------------------------------- mongosh

// The shell sample is two files: mongosh is a JavaScript runtime, so the program is a .js and the
// .sh beside it is only the invocation — which is also the thing worth reading, because every TLS
// flag lives there rather than in the script.
const scMongoshShell = `#!/usr/bin/env bash
{{.Header "# "}}

set -euo pipefail

echo "Connecting to {{.Addr}}..."
mongosh {{.MongoURI | sq}} \
{{- if .Verify}}
  --tls --tlsCAFile {{.CA | sq}} \
{{- else if .Encrypted}}
  --tls --tlsAllowInvalidCertificates \
{{- end}}
{{- if .MTLS}}
  --tlsCertificateKeyFile {{.ClientPEM | sq}} \
{{- end}}
  --quiet --file crud.js
`

const scMongoshJS = `{{.Header "// "}}

const DATABASE = {{.Database | q}}
const COLLECTION = {{.Collection | q}}

const build = db.getSiblingDB('admin').runCommand({ buildInfo: 1 })
print('Connected to {{.Target.Host}}:{{.Target.Port}} - MongoDB ' + build.version)
{{- if .Ops.Schema}}

const customers = db.getSiblingDB(DATABASE).getCollection(COLLECTION)
customers.createIndex({ email: 1 }, { unique: true })
print('Collection ready: ' + DATABASE + '.' + COLLECTION)
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

print('{{if .Ops.Create}}CREATE{{else}}SEED{{end}}')
const customerId = 1
customers.replaceOne(
  { _id: customerId },
  { name: {{scDemoName | q}}, email: {{scDemoEmail | q}}, created_at: new Date() },
  { upsert: true },
)
print('Created customer ' + customerId + ': {{scDemoName}}')
{{- end}}
{{- if .Ops.Read}}

print('READ')
customers.find({ _id: customerId }).forEach((doc) => print([doc._id, doc.name, doc.email].join(' | ')))
{{- end}}
{{- if .Ops.Update}}

print('UPDATE')
const upd = customers.updateOne({ _id: customerId }, { $set: { email: {{scDemoNewEmail | q}} } })
print('Updated customer ' + customerId + ' (' + upd.modifiedCount + ' document)')
const after = customers.findOne({ _id: customerId })
print([after._id, after.name, after.email].join(' | '))
{{- end}}
{{- if .Ops.Delete}}

print('DELETE')
const del = customers.deleteOne({ _id: customerId })
print('Deleted customer ' + customerId + ' (' + del.deletedCount + ' document)')
print('Documents with that id now: ' + customers.countDocuments({ _id: customerId }))
{{- end}}

print('{{.Scenario.Label}} example completed successfully.')
`

// ------------------------------------------------------------------------------- C#

const scMongoCS = `{{.Header "// "}}

using MongoDB.Bson;
using MongoDB.Driver;
{{- if or .Verify .MTLS}}
using System.Security.Cryptography.X509Certificates;
{{- end}}
{{- if .Verify}}
using System.Net.Security;
{{- end}}
{{- if .Ops.Any}}

const string Database = {{.Database | cs}};
const string Collection = {{.Collection | cs}};
{{- end}}

// The URI carries the credentials and the shape of the endpoint; TLS is set on the
// settings object below, because the .NET driver takes no CA file in the URI.
var settings = MongoClientSettings.FromConnectionString({{.MongoURI | cs}});
settings.ConnectTimeout = TimeSpan.FromSeconds(10);
settings.ServerSelectionTimeout = TimeSpan.FromSeconds(10);
{{- if .Encrypted}}
settings.UseTls = true;
{{- if .Verify}}
// The driver validates through .NET's SslStream, which knows only the system trust
// store. The callback below checks the chain against the stack CA alone and still
// refuses a certificate that does not name this host.
var stackCa = X509Certificate2.CreateFromPem(File.ReadAllText({{.CA | cs}}));
{{- else}}
// Encrypted, but the server is not identified.
settings.AllowInsecureTls = true;
{{- end}}
settings.SslSettings = new SslSettings
{
    CheckCertificateRevocation = false,
{{- if .Verify}}
    ServerCertificateValidationCallback = (_, certificate, _, errors) => IssuedByStackCa(certificate, errors),
{{- end}}
{{- if .MTLS}}
    // Mutual TLS: the certificate this client presents, issued by the stack CA.
    ClientCertificates = new X509Certificate[]
    {
        X509Certificate2.CreateFromPemFile({{.ClientCert | cs}}, {{.ClientKey | cs}}),
    },
{{- end}}
};
{{- end}}

var client = new MongoClient(settings);

var build = await client.GetDatabase("admin").RunCommandAsync<BsonDocument>(new BsonDocument("buildInfo", 1));
Console.WriteLine($"Connected to {{.Target.Host}}:{{.Target.Port}} - MongoDB {build["version"]}");
{{- if .Ops.Schema}}

var customers = client.GetDatabase(Database).GetCollection<BsonDocument>(Collection);
await customers.Indexes.CreateOneAsync(new CreateIndexModel<BsonDocument>(
    Builders<BsonDocument>.IndexKeys.Ascending("email"),
    new CreateIndexOptions { Unique = true }));
Console.WriteLine($"Collection ready: {Database}.{Collection}");
{{- end}}
{{- if or .Ops.Create .Ops.Seed}}

Console.WriteLine("{{if .Ops.Create}}CREATE{{else}}SEED{{end}}");
var customerId = 1;
var byId = Builders<BsonDocument>.Filter.Eq("_id", customerId);
await customers.ReplaceOneAsync(byId,
    new BsonDocument
    {
        ["_id"] = customerId,
        ["name"] = {{scDemoName | cs}},
        ["email"] = {{scDemoEmail | cs}},
        ["created_at"] = DateTime.UtcNow,
    },
    new ReplaceOptions { IsUpsert = true });
Console.WriteLine($"Created customer {customerId}: {{scDemoName}}");
{{- end}}
{{- if .Ops.Read}}

Console.WriteLine("READ");
foreach (var doc in await customers.Find(byId).ToListAsync())
{
    Console.WriteLine($"{doc["_id"]} | {doc["name"]} | {doc["email"]}");
}
{{- end}}
{{- if .Ops.Update}}

Console.WriteLine("UPDATE");
var updated = await customers.UpdateOneAsync(byId, Builders<BsonDocument>.Update.Set("email", {{scDemoNewEmail | cs}}));
Console.WriteLine($"Updated customer {customerId} ({updated.ModifiedCount} document)");
var after = await customers.Find(byId).FirstAsync();
Console.WriteLine($"{after["_id"]} | {after["name"]} | {after["email"]}");
{{- end}}
{{- if .Ops.Delete}}

Console.WriteLine("DELETE");
var deleted = await customers.DeleteOneAsync(byId);
Console.WriteLine($"Deleted customer {customerId} ({deleted.DeletedCount} document)");
Console.WriteLine($"Documents with that id now: {await customers.CountDocumentsAsync(byId)}");
{{- end}}

Console.WriteLine("{{.Scenario.Label}} example completed successfully.");
{{- if .Verify}}

bool IssuedByStackCa(X509Certificate? certificate, SslPolicyErrors errors)
{
    if (certificate is null || errors.HasFlag(SslPolicyErrors.RemoteCertificateNameMismatch))
    {
        return false;
    }
    using var chain = new X509Chain();
    chain.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
    chain.ChainPolicy.CustomTrustStore.Add(stackCa);
    // A lab CA publishes no revocation list, so there is nothing to ask.
    chain.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
    return chain.Build(new X509Certificate2(certificate));
}
{{- end}}
`
