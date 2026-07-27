package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Check Work functions for the second batch of 20 PS MongoDB labs
// (app/labs_mongodb2.go). Reuses every shared helper from labs_mongodb.go
// (mongoLabFrameFromStack, mongoLabFrameMembers, mongoLabReplStatus,
// mongoLabExplain, mongoLabMongos, chunkCount, planStage, toInt64, etc).

// mongoLabHostToNode maps a replica-set config "host" string (e.g.
// "rs-1.example.net:27017", built by mongoInitReplicaSet as
// fqdnOf(hosts[n.ID],domain)+":"+mongoPort) back to its design node ID, for a
// given frame's members.
func mongoLabHostToNode(doc designDoc, frame designFrame) map[string]string {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	out := map[string]string{}
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != frame.Type {
			continue
		}
		out[fqdnOf(hosts[n.ID], domain)+":27017"] = n.ID
	}
	return out
}

// mongoLabReplConfig runs replSetGetConfig on c and returns the raw config doc.
func (a *App) mongoLabReplConfig(ctx context.Context, c dbConn) (bson.M, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return nil, err
	}
	defer closer()
	var out bson.M
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&out)
	if err != nil {
		return nil, err
	}
	cfg, _ := out["config"].(bson.M)
	if cfg == nil {
		return nil, errors.New("no config in replSetGetConfig response")
	}
	return cfg, nil
}

// mongoLabPrimary returns the dbConn + node ID of whichever member of frame is
// currently PRIMARY, or ok=false if none is reachable/elected yet.
func (a *App) mongoLabPrimary(ctx context.Context, st Stack, doc designDoc, frame designFrame) (dbConn, string, bool) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return dbConn{}, "", false
	}
	for _, m := range a.mongoLabFrameMembers(st, doc, frame, "") {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err != nil || status.MyState != 1 {
			continue
		}
		return m, nodeIDForContainer(deps, m.ContainerID), true
	}
	return dbConn{}, "", false
}

// mongoLabExec runs cmd inside a lab node's container and returns stdout,
// trimmed, on success — the fallback for the handful of checks (TLS, GridFS
// file comparison, PBM's own CLI) that don't fit the driver-based model.
func (a *App) mongoLabExec(ctx context.Context, containerID string, cmd []string) (string, bool) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, cmd, nil)
	if err != nil || res.Code != 0 {
		return "", false
	}
	return strings.TrimSpace(res.Stdout), true
}

// ---------------------------------------------------------- indexing strategies

func (a *App) checkMongoMultikeyIndex(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	explain, err := a.mongoLabExplain(ctx, c, "labdb",
		bson.D{{Key: "find", Value: "products"}, {Key: "filter", Value: bson.D{{Key: "tags", Value: "y"}}}}, "queryPlanner")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run explain — has `labdb.products` been created with a document whose tags include \"y\"?"}
	}
	if !findMultiKey(explain) {
		return LabStepResult{Passed: false, Message: "The scan isn't reporting isMultiKey:true yet — create the index with `db.products.createIndex({tags:1})`."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the index on tags is multikey."}
}

// findMultiKey searches an explain() result for any stage reporting
// isMultiKey:true, without assuming a fixed nesting depth.
func findMultiKey(v any) bool {
	switch t := v.(type) {
	case bson.M:
		if b, ok := t["isMultiKey"].(bool); ok && b {
			return true
		}
		for _, vv := range t {
			if findMultiKey(vv) {
				return true
			}
		}
	case bson.A:
		for _, vv := range t {
			if findMultiKey(vv) {
				return true
			}
		}
	}
	return false
}

func (a *App) checkMongoPartialIndex(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	cur, err := client.Database("labdb").Collection("products").Indexes().List(ctx)
	if err != nil {
		return LabStepResult{Passed: false, Message: "The `labdb.products` collection doesn't exist yet — create the index first."}
	}
	var idxs []bson.M
	cur.All(ctx, &idxs)
	for _, ix := range idxs {
		if pf, ok := ix["partialFilterExpression"].(bson.M); ok {
			if v, ok := pf["inStock"]; ok && v == true {
				return LabStepResult{Passed: true, Message: "Confirmed: a partial index with partialFilterExpression {inStock:true} exists."}
			}
		}
	}
	return LabStepResult{Passed: false, Message: "No partial index with `partialFilterExpression:{inStock:true}` found yet."}
}

// ---------------------------------------------------------- schema validation

func (a *App) checkMongoValidatorCreated(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	specs, err := client.Database("labdb").ListCollectionSpecifications(ctx, bson.M{"name": "orders"})
	if err != nil || len(specs) == 0 {
		return LabStepResult{Passed: false, Message: "The `labdb.orders` collection doesn't exist yet — create it with a $jsonSchema validator."}
	}
	var opts struct {
		Validator bson.M `bson:"validator"`
	}
	bson.Unmarshal(specs[0].Options, &opts)
	if _, ok := opts.Validator["$jsonSchema"]; !ok {
		return LabStepResult{Passed: false, Message: "`labdb.orders` exists but has no `$jsonSchema` validator — recreate it with one."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labdb.orders has a $jsonSchema validator."}
}

func (a *App) checkMongoValidationEnforced(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	// Confirm the validator actually exists first — an insert probe against a
	// collection that doesn't exist yet would silently *create* it without one
	// (MongoDB auto-creates on first insert), permanently blocking the previous
	// step from ever being able to create it validated.
	specs, err := client.Database("labdb").ListCollectionSpecifications(ctx, bson.M{"name": "orders"})
	if err != nil || len(specs) == 0 {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — labdb.orders doesn't exist yet."}
	}
	var opts struct {
		Validator bson.M `bson:"validator"`
	}
	bson.Unmarshal(specs[0].Options, &opts)
	if _, ok := opts.Validator["$jsonSchema"]; !ok {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — labdb.orders has no $jsonSchema validator yet."}
	}
	_, err = client.Database("labdb").Collection("orders").InsertOne(ctx, bson.M{"amount": 10})
	if err == nil {
		return LabStepResult{Passed: false, Message: "An insert missing `orderId` was accepted — the validator isn't rejecting invalid documents."}
	}
	if !isMongoValidationError(err) {
		return LabStepResult{Passed: false, Message: "The insert failed, but not with the expected document-validation error — got: " + err.Error()}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: an invalid document was rejected by the $jsonSchema validator."}
}

func isMongoValidationError(err error) bool {
	var we mongo.WriteException
	if errors.As(err, &we) {
		for _, we2 := range we.WriteErrors {
			if we2.Code == 121 { // DocumentValidationFailure
				return true
			}
		}
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code == 121
	}
	return false
}

// ---------------------------------------------------------------------- TLS

func (a *App) checkMongoTLSEnabled(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	out, ok := a.mongoLabExec(ctx, c.ContainerID, []string{"mongosh", "--host", "psm-1", "--quiet", "--tls",
		"--tlsCAFile=/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt",
		"-u", "admin", "-p", "admin_password", "--authenticationDatabase", "admin",
		"--eval", "db.adminCommand({ping:1}).ok"})
	if !ok || strings.TrimSpace(out) != "1" {
		return LabStepResult{Passed: false, Message: "A TLS connection isn't succeeding yet — add the net.tls block to mongod.conf and restart mongod."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: mongod accepts a TLS connection."}
}

func (a *App) checkMongoTLSRequired(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	// A non-TLS connection should now fail outright.
	_, plainOK := a.mongoLabExec(ctx, c.ContainerID, []string{"mongosh", "--quiet",
		"-u", "admin", "-p", "admin_password", "--authenticationDatabase", "admin",
		"--eval", "db.adminCommand({ping:1}).ok"})
	if plainOK {
		return LabStepResult{Passed: false, Message: "A non-TLS connection still succeeds — set tlsMode to requireTLS."}
	}
	out, tlsOK := a.mongoLabExec(ctx, c.ContainerID, []string{"mongosh", "--host", "psm-1", "--quiet", "--tls",
		"--tlsCAFile=/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt",
		"-u", "admin", "-p", "admin_password", "--authenticationDatabase", "admin",
		"--eval", "db.adminCommand({ping:1}).ok"})
	if !tlsOK || strings.TrimSpace(out) != "1" {
		return LabStepResult{Passed: false, Message: "A TLS connection should still work with requireTLS — confirm mongod is still running."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: non-TLS connections are refused, and TLS connections still work."}
}

// -------------------------------------------------------- profiler/currentOp

func (a *App) checkMongoProfilerCaught(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	n, err := client.Database("labdb").Collection("system.profile").CountDocuments(ctx, bson.M{})
	if err != nil || n == 0 {
		return LabStepResult{Passed: false, Message: "No profiled operations found yet — enable profiling with `db.setProfilingLevel(1,{slowms:0})` and run a query."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d operation(s) recorded in labdb.system.profile.", n)}
}

func (a *App) checkMongoHungOpKilled(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	var out bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "currentOp", Value: 1}}).Decode(&out); err != nil {
		return LabStepResult{Passed: false, Message: "Could not run currentOp."}
	}
	ops, _ := out["inprog"].(bson.A)
	for _, o := range ops {
		om, ok := o.(bson.M)
		if !ok {
			continue
		}
		if cmd, ok := om["command"].(bson.M); ok {
			if _, ok := cmd["aggregate"]; ok {
				if s, _ := cmd["aggregate"].(string); s == "hang" {
					return LabStepResult{Passed: false, Message: "The hung operation is still running — find its opid with currentOp and run db.killOp(opid)."}
				}
			}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: no hung `hang` aggregation is running anymore."}
}

// ------------------------------------------------------------ aggregation

func (a *App) checkMongoAggregationPipeline(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	want := map[string]int64{"a": 25, "b": 25, "c": 7}
	got, ok := aggregateSalesByCategory(ctx, client)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not aggregate labdb.sales — seed the fixed dataset from the instructions first."}
	}
	for k, v := range want {
		if got[k] != v {
			return LabStepResult{Passed: false, Message: fmt.Sprintf("Category %q totals %d in labdb.sales, expected %d — reinsert the exact dataset from the instructions.", k, got[k], v)}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labdb.sales groups correctly by category."}
}

func aggregateSalesByCategory(ctx context.Context, client *mongo.Client) (map[string]int64, bool) {
	cur, err := client.Database("labdb").Collection("sales").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$category"}, {Key: "total", Value: bson.D{{Key: "$sum", Value: "$amount"}}}}}},
	})
	if err != nil {
		return nil, false
	}
	defer cur.Close(ctx)
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, false
	}
	out := map[string]int64{}
	for _, r := range rows {
		id, _ := r["_id"].(string)
		n, _ := toInt64(r["total"])
		out[id] = n
	}
	return out, true
}

func (a *App) checkMongoMergeIntoCollection(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	cur, err := client.Database("labdb").Collection("salesSummary").Find(ctx, bson.M{})
	if err != nil {
		return LabStepResult{Passed: false, Message: "labdb.salesSummary doesn't exist yet — run the pipeline with a $merge stage."}
	}
	defer cur.Close(ctx)
	var rows []bson.M
	cur.All(ctx, &rows)
	want := map[string]int64{"a": 25, "b": 25, "c": 7}
	got := map[string]int64{}
	for _, r := range rows {
		id, _ := r["_id"].(string)
		n, _ := toInt64(r["total"])
		got[id] = n
	}
	for k, v := range want {
		if got[k] != v {
			return LabStepResult{Passed: false, Message: fmt.Sprintf("labdb.salesSummary has %d for category %q, expected %d.", got[k], k, v)}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labdb.salesSummary holds the merged totals."}
}

// ----------------------------------------------------------------- GridFS

func (a *App) checkMongoGridFSUploaded(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	n, err := client.Database("labdb").Collection("fs.files").CountDocuments(ctx,
		bson.M{"filename": "upload-me.txt"})
	if err != nil || n == 0 {
		return LabStepResult{Passed: false, Message: "No file uploaded to GridFS yet — run `mongofiles ... put /etc/hostname`."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a file is stored in labdb.fs.files."}
}

func (a *App) checkMongoGridFSDownloaded(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, c.ContainerID, []string{"cmp", "/tmp/upload-me.txt", "/tmp/download/upload-me.txt"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "The downloaded copy doesn't match (or doesn't exist yet) — run `mongofiles ... get upload-me.txt` into /tmp/download."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the file downloaded from GridFS is byte-identical to the original."}
}

// --------------------------------------------------------- election priorities

func (a *App) checkMongoPrioritiesSet(ctx context.Context, run LabRun, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	c, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	cfg, err := a.mongoLabReplConfig(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read the replica set configuration."}
	}
	members, _ := cfg["members"].(bson.A)
	hostToNode := mongoLabHostToNode(doc, frame)
	var favored string
	maxP, minP := -1.0, 1e9
	for _, m := range members {
		mm, ok := m.(bson.M)
		if !ok {
			continue
		}
		p, _ := toFloat64(mm["priority"])
		if p > maxP {
			maxP = p
			host, _ := mm["host"].(string)
			favored = hostToNode[host]
		}
		if p < minP {
			minP = p
		}
	}
	if favored == "" || maxP <= minP {
		return LabStepResult{Passed: false, Message: "No member has a strictly higher priority than the rest yet."}
	}
	a.store.SetLabRunLeader(run.ID, favored)
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, favored) + " has the highest priority."}
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func (a *App) checkMongoPriorityWins(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — no favored member has been recorded yet."}
	}
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	_, primaryNode, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the election to finish."}
	}
	if primaryNode != run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, primaryNode) + " is PRIMARY, not " + nodeLabel(doc, run.InitialLeaderNode) + " — step down the primary and let the favored member win."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, primaryNode) + ", the favored member, is PRIMARY."}
}

// ------------------------------------------------------------- change streams

func (a *App) checkMongoChangeStreamCaptured(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	c, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the replica set."}
	}
	defer closer()
	cur, err := client.Database("labdb").Collection("changeLog").Distinct(ctx, "op", bson.M{})
	if err != nil {
		return LabStepResult{Passed: false, Message: "labdb.changeLog doesn't exist yet — start the background watcher and generate some writes."}
	}
	seen := map[string]bool{}
	for _, v := range cur {
		if s, ok := v.(string); ok {
			seen[s] = true
		}
	}
	var missing []string
	for _, want := range []string{"insert", "update", "delete"} {
		if !seen[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return LabStepResult{Passed: false, Message: "Still missing: " + strings.Join(missing, ", ") + " — generate that kind of write and give the watcher a moment to catch up."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: insert, update, and delete were all captured by the change stream watcher."}
}

// -------------------------------------------------------------- transactions

func (a *App) checkMongoTransactionCommitted(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	c, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	balA, balB, ok := readAccountBalances(ctx, a, c)
	if !ok {
		return LabStepResult{Passed: false, Message: "labdb.accounts doesn't have both A and B yet — seed them first."}
	}
	if balA != 400 || balB != 600 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("A=%d, B=%d — run the transaction moving 100 from A to B.", balA, balB)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: A=400, B=600 — the transfer committed atomically."}
}

func readAccountBalances(ctx context.Context, a *App, c dbConn) (int64, int64, bool) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return 0, 0, false
	}
	defer closer()
	var docA, docB bson.M
	if err := client.Database("labdb").Collection("accounts").FindOne(ctx, bson.M{"_id": "A"}).Decode(&docA); err != nil {
		return 0, 0, false
	}
	if err := client.Database("labdb").Collection("accounts").FindOne(ctx, bson.M{"_id": "B"}).Decode(&docB); err != nil {
		return 0, 0, false
	}
	balA, _ := toInt64(docA["balance"])
	balB, _ := toInt64(docB["balance"])
	return balA, balB, true
}

func (a *App) checkMongoTransactionAborted(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	c, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	balA, balB, ok := readAccountBalances(ctx, a, c)
	if !ok {
		return LabStepResult{Passed: false, Message: "labdb.accounts doesn't have both A and B yet — complete the previous step first."}
	}
	if balA != 400 || balB != 600 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("A=%d, B=%d — expected them unchanged at 400/600. Either the abort didn't run, or it didn't actually abort.", balA, balB)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: A and B are unchanged — the aborted transaction left no trace."}
}

// ------------------------------------------------------------ arbiter/quorum

func (a *App) checkMongoArbiterAdded(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	c, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	cfg, err := a.mongoLabReplConfig(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read the replica set configuration."}
	}
	members, _ := cfg["members"].(bson.A)
	for _, m := range members {
		mm, ok := m.(bson.M)
		if !ok {
			continue
		}
		if b, _ := mm["arbiterOnly"].(bool); b {
			return LabStepResult{Passed: true, Message: "Confirmed: an arbiter-only member is part of the replica set."}
		}
	}
	return LabStepResult{Passed: false, Message: "No arbiter-only member found yet — run rs.addArb(\"spare:27017\") from the primary."}
}

func (a *App) checkMongoQuorumSurvivedLoss(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	_, _, ok = a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — the cluster may have actually lost quorum. Confirm the arbiter was added and only one secondary is stopped."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a PRIMARY is still reachable with one data-bearing member down — the arbiter's vote kept quorum."}
}

// ------------------------------------------------------------ hidden/delayed

func (a *App) checkMongoHiddenDelayedConfigured(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	c, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	cfg, err := a.mongoLabReplConfig(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read the replica set configuration."}
	}
	members, _ := cfg["members"].(bson.A)
	for _, m := range members {
		mm, ok := m.(bson.M)
		if !ok {
			continue
		}
		hidden, _ := mm["hidden"].(bool)
		prio, _ := toFloat64(mm["priority"])
		delay, hasDelay := toInt64(mm["secondaryDelaySecs"])
		if hidden && prio == 0 && hasDelay && delay > 0 {
			return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: a member is hidden, priority 0, and delayed %ds.", delay)}
		}
	}
	return LabStepResult{Passed: false, Message: "No member is configured hidden + priority 0 + secondaryDelaySecs yet."}
}

func (a *App) checkMongoHiddenDelayedVerified(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	primaryConn, _, ok := a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the replica set to settle."}
	}
	cfg, err := a.mongoLabReplConfig(ctx, primaryConn)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read the replica set configuration."}
	}
	members, _ := cfg["members"].(bson.A)
	var hiddenHost string
	for _, m := range members {
		mm, ok := m.(bson.M)
		if !ok {
			continue
		}
		if hidden, _ := mm["hidden"].(bool); hidden {
			hiddenHost, _ = mm["host"].(string)
		}
	}
	if hiddenHost == "" {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — no member is hidden yet."}
	}
	hostToNode := mongoLabHostToNode(doc, frame)
	hiddenNodeID := hostToNode[hiddenHost]
	hiddenConn, hok := a.dbConnFor(st, hiddenNodeID)
	if !hok {
		return LabStepResult{Passed: false, Message: "Could not reach the hidden member."}
	}

	primaryClient, closer1, err := a.mongoClientFor(ctx, primaryConn)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the primary."}
	}
	defer closer1()
	markerID := "hiddenDelayMarker"
	if _, err := primaryClient.Database("labdb").Collection("hiddenTest").InsertOne(ctx, bson.M{"_id": markerID}); err != nil {
		return LabStepResult{Passed: false, Message: "Could not write the marker document."}
	}

	hiddenClient, closer2, err := a.mongoClientFor(ctx, hiddenConn)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the hidden member."}
	}
	defer closer2()
	count, _ := hiddenClient.Database("labdb").Collection("hiddenTest").CountDocuments(ctx, bson.M{"_id": markerID})
	if count > 0 {
		return LabStepResult{Passed: false, Message: "The marker document already replicated to the delayed member — secondaryDelaySecs isn't holding it back."}
	}

	var hello bson.M
	if err := primaryClient.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return LabStepResult{Passed: false, Message: "Could not run hello against the primary."}
	}
	hostList, _ := hello["hosts"].(bson.A)
	for _, h := range hostList {
		if s, _ := h.(string); s == hiddenHost {
			return LabStepResult{Passed: false, Message: "The hidden member still appears in hello's hosts list."}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the marker document hasn't reached the delayed member yet, and it's excluded from hello's host list."}
}

// -------------------------------------------------------------------- PBM

// pbmListEntry is the minimal shape `pbm list -o json` reports per backup.
type pbmListEntry struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type pbmListOutput struct {
	Snapshots []pbmListEntry `json:"snapshots"`
}

func (a *App) pbmHasDoneBackup(ctx context.Context, containerID string) (bool, string) {
	out, ok := a.mongoLabExec(ctx, containerID, []string{"bash", "-c", "export $(cat /etc/sysconfig/pbm-agent 2>/dev/null || cat /etc/default/pbm-agent 2>/dev/null); pbm list -o json"})
	if !ok {
		return false, ""
	}
	var parsed pbmListOutput
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return false, out
	}
	for _, s := range parsed.Snapshots {
		if s.Status == "done" {
			return true, out
		}
	}
	return false, out
}

func (a *App) checkMongoPBMFullBackup(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	if len(members) == 0 {
		return LabStepResult{Passed: false, Message: "No replica set members are running yet."}
	}
	if done, _ := a.pbmHasDoneBackup(ctx, members[0].ContainerID); done {
		return LabStepResult{Passed: true, Message: "Confirmed: PBM reports a completed backup."}
	}
	return LabStepResult{Passed: false, Message: "No completed PBM backup found yet — run `pbm backup` and wait for it to finish."}
}

func (a *App) checkMongoPBMPitrEnabled(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	if len(members) == 0 {
		return LabStepResult{Passed: false, Message: "No replica set members are running yet."}
	}
	out, ok := a.mongoLabExec(ctx, members[0].ContainerID, []string{"bash", "-c",
		"export $(cat /etc/sysconfig/pbm-agent 2>/dev/null || cat /etc/default/pbm-agent 2>/dev/null); pbm config"})
	if !ok || !strings.Contains(out, "enabled: true") {
		return LabStepResult{Passed: false, Message: "PITR isn't enabled yet — run `pbm config --set pitr.enabled=true`."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: PITR is enabled."}
}

func (a *App) checkMongoPBMShardedBackup(ctx context.Context, st Stack) LabStepResult {
	// pbm-agent only runs on data-bearing members — mongos has no local data and
	// never gets a real PBM_MONGODB_URI (its /etc/sysconfig/pbm-agent is an
	// unfilled template), so the pbm CLI has to be driven from a shard member.
	doc, frame, ok := mongoLabConfigFrame(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No sharded cluster found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "shard")
	if len(members) == 0 {
		return LabStepResult{Passed: false, Message: "No shard members are running yet."}
	}
	if done, _ := a.pbmHasDoneBackup(ctx, members[0].ContainerID); done {
		return LabStepResult{Passed: true, Message: "Confirmed: PBM reports a completed cluster-wide backup."}
	}
	return LabStepResult{Passed: false, Message: "No completed PBM backup found yet — run `pbm backup` from a shard member's terminal and wait for it to finish."}
}

// --------------------------------------------------------------- rollback

func (a *App) checkMongoRollbackDivergence(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	_, _, ok = a.mongoLabPrimary(ctx, st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable yet among the members that were secondaries — give the election a moment."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a new PRIMARY has been elected."}
}

func (a *App) checkMongoRollbackCompleted(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	if len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Not all three members are reachable yet — has the old primary rejoined?"}
	}
	for _, m := range members {
		client, closer, err := a.mongoClientFor(ctx, m)
		if err != nil {
			return LabStepResult{Passed: false, Message: "Could not reach every member yet — the old primary may still be rejoining."}
		}
		count, cErr := client.Database("labdb").Collection("rollbacktest").CountDocuments(ctx, bson.M{"_id": "willroll"})
		closer()
		if cErr == nil && count > 0 {
			return LabStepResult{Passed: false, Message: "The un-replicated document is still present on at least one member — the old primary may not have rejoined and rolled back yet."}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the un-replicated document is gone from every member — it was rolled back."}
}

// ------------------------------------------------------------- sharded: balancer

func (a *App) checkMongoBalancerOn(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	var out bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "balancerStatus", Value: 1}}).Decode(&out); err != nil {
		return LabStepResult{Passed: false, Message: "Could not run balancerStatus."}
	}
	mode, _ := out["mode"].(string)
	if mode == "off" {
		return LabStepResult{Passed: false, Message: "The balancer is off — this cluster's balancer should be on by default; something disabled it."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the balancer is enabled (mode=" + mode + ")."}
}

func (a *App) checkMongoMigrationOccurred(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	n, err := client.Database("config").Collection("changelog").CountDocuments(ctx, bson.M{
		"ns":   "labdb.balanced",
		"what": bson.M{"$regex": "^moveChunk"},
	})
	if err != nil || n == 0 {
		return LabStepResult{Passed: false, Message: "No moveChunk entries found in config.changelog yet for labdb.balanced — shard it, shrink its chunk size, insert a spread of data, and give the balancer a minute."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d moveChunk event(s) recorded for labdb.balanced.", n)}
}

// -------------------------------------------------------- sharded: config RS

func (a *App) checkMongoConfigRSObserved(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabConfigFrame(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No sharded cluster found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "config")
	if len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Not all three config server members are running yet."}
	}
	primaries, secondaries := 0, 0
	for _, m := range members {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err != nil {
			continue
		}
		switch status.MyState {
		case 1:
			primaries++
		case 2:
			secondaries++
		}
	}
	if primaries != 1 || secondaries != 2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected exactly 1 PRIMARY and 2 SECONDARY among the config servers, found %d PRIMARY and %d SECONDARY.", primaries, secondaries)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the config server replica set has 1 PRIMARY and 2 healthy SECONDARY."}
}

// mongoLabConfigFrame finds the psmdb frame in this lab's stack (any setup).
func mongoLabConfigFrame(st Stack) (designDoc, designFrame, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return designDoc{}, designFrame{}, false
	}
	for _, f := range doc.Frames {
		if f.Type == "psmdb" {
			return doc, f, true
		}
	}
	return doc, designFrame{}, false
}

func (a *App) checkMongoConfigRSSurvived(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabConfigFrame(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No sharded cluster found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "config")
	primaries := 0
	for _, m := range members {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err == nil && status.MyState == 1 {
			primaries++
		}
	}
	if primaries != 1 {
		return LabStepResult{Passed: false, Message: "No new PRIMARY has been elected among the remaining config servers yet."}
	}
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos — the cluster may not have survived the config server outage."}
	}
	defer closer()
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "listDatabases", Value: 1}}).Err(); err != nil {
		return LabStepResult{Passed: false, Message: "mongos couldn't answer a basic admin command."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a new config-server PRIMARY was elected and the cluster kept working through mongos."}
}

// ------------------------------------------------------------- zone sharding

func (a *App) checkMongoZonesTagged(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	n, err := client.Database("config").Collection("tags").CountDocuments(ctx, bson.M{"ns": "labdb.zoned"})
	if err != nil || n < 3 {
		return LabStepResult{Passed: false, Message: "Fewer than 3 zone key ranges found for labdb.zoned yet — tag all three shards and assign all three zone ranges."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: 3 zone key ranges are assigned for labdb.zoned."}
}

func (a *App) checkMongoZonePinned(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	var tag bson.M
	if err := client.Database("config").Collection("tags").FindOne(ctx, bson.M{"ns": "labdb.zoned", "tag": "north"}).Decode(&tag); err != nil {
		return LabStepResult{Passed: false, Message: "No zone range tagged \"north\" found yet — complete the previous step first."}
	}
	var shardDoc bson.M
	if err := client.Database("config").Collection("shards").FindOne(ctx, bson.M{"tags": "north"}).Decode(&shardDoc); err != nil {
		return LabStepResult{Passed: false, Message: "No shard is tagged \"north\" yet."}
	}
	wantShard, _ := shardDoc["_id"].(string)

	// executionStats' per-shard nReturned (not just which shards the router
	// considered) is the real proof of where the data lives: mongos can list a
	// shard in the routing plan just because a chunk's range formally overlaps
	// the query (e.g. an empty boundary/gap chunk sitting at a zone's edge)
	// while that shard actually returns zero matching documents — checking only
	// queryPlanner.winningPlan.shards's length flags that harmless routing
	// conservatism as a zone violation it isn't.
	explain, err := a.mongoLabExplain(ctx, c, "labdb",
		bson.D{{Key: "find", Value: "zoned"}, {Key: "filter", Value: bson.D{{Key: "region", Value: "north"}}}}, "executionStats")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run explain — insert some data with region \"north\" first."}
	}
	es, _ := explain["executionStats"].(bson.M)
	exStages, _ := es["executionStages"].(bson.M)
	perShard, _ := exStages["shards"].(bson.A)
	if len(perShard) == 0 {
		return LabStepResult{Passed: false, Message: "Could not read per-shard execution stats — insert some data with region \"north\" first."}
	}
	totalReturned := int64(0)
	for _, s := range perShard {
		sm, ok := s.(bson.M)
		if !ok {
			continue
		}
		shardName, _ := sm["shardName"].(string)
		stages, _ := sm["executionStages"].(bson.M)
		n, _ := toInt64(stages["nReturned"])
		totalReturned += n
		if n > 0 && shardName != wantShard {
			return LabStepResult{Passed: false, Message: fmt.Sprintf("Shard %s returned %d document(s) for region \"north\", but that region is zoned to %s.", shardName, n, wantShard)}
		}
	}
	if totalReturned == 0 {
		return LabStepResult{Passed: false, Message: "No documents with region \"north\" found yet — insert some data first."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: all %d document(s) for the \"north\" region came from %s, exactly the shard it's zoned to.", totalReturned, wantShard)}
}

// --------------------------------------------------------- add/remove shard

func (a *App) checkMongoShardAdded(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	n, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{})
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read config.shards."}
	}
	if n < 4 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Only %d shard(s) registered — run sh.addShard with the spare replica set's connection string.", n)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d shards are registered.", n)}
}

func (a *App) checkMongoShardRemoved(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	n, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{"_id": "lab-spare-rs"})
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read config.shards."}
	}
	if n > 0 {
		return LabStepResult{Passed: false, Message: "lab-spare-rs is still registered as a shard — keep running db.adminCommand({removeShard:\"lab-spare-rs\"}) until state reads \"completed\"."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: lab-spare-rs has been fully removed from the cluster."}
}

// -------------------------------------------------------------- jumbo chunk

func (a *App) checkMongoJumboChunkFormed(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	var collDoc bson.M
	if err := client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": "labdb.jumbo"}).Decode(&collDoc); err != nil {
		return LabStepResult{Passed: false, Message: "labdb.jumbo isn't sharded yet."}
	}
	uuid := collDoc["uuid"]
	n, err := client.Database("config").Collection("chunks").CountDocuments(ctx, bson.M{"uuid": uuid, "jumbo": true})
	if err != nil || n == 0 {
		return LabStepResult{Passed: false, Message: "No jumbo chunk found yet — insert several thousand documents all sharing the same tenantId and give the auto-splitter a couple minutes to give up."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d chunk(s) flagged jumbo on labdb.jumbo.", n)}
}

func (a *App) checkMongoShardKeyRefined(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	var collDoc bson.M
	if err := client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": "labdb.jumbo"}).Decode(&collDoc); err != nil {
		return LabStepResult{Passed: false, Message: "labdb.jumbo isn't sharded yet."}
	}
	key, _ := collDoc["key"].(bson.M)
	if _, ok := key["_id"]; !ok {
		return LabStepResult{Passed: false, Message: "The shard key doesn't include `_id` yet — create the compound index and run refineCollectionShardKey."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labdb.jumbo's shard key now includes _id as a differentiator."}
}

// -------------------------------------------------------------- resharding

func (a *App) checkMongoReshardedToNewKey(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	var collDoc bson.M
	if err := client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": "labdb.reshardme"}).Decode(&collDoc); err != nil {
		return LabStepResult{Passed: false, Message: "labdb.reshardme isn't sharded yet."}
	}
	key, _ := collDoc["key"].(bson.M)
	if _, ok := key["newKey"]; !ok {
		return LabStepResult{Passed: false, Message: "The shard key is still the old one — run reshardCollection and wait for it to finish."}
	}
	if _, ok := key["oldKey"]; ok {
		return LabStepResult{Passed: false, Message: "The shard key still includes oldKey — resharding hasn't fully replaced it yet."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labdb.reshardme is now sharded on {newKey:1}."}
}
