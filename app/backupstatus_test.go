package main

import "testing"

func TestParsePgBackRestInfo(t *testing.T) {
	out := `[{"name":"pb","backup":[
	  {"label":"20261008-140000F","type":"full","timestamp":{"start":1791468000,"stop":1791468060},"info":{"size":31000000}},
	  {"label":"20261008-150000F_20261008-151000I","type":"incr","timestamp":{"start":1791471600,"stop":1791471620},"info":{"size":2000}}],
	  "status":{"code":0,"message":"ok","lock":{"backup":{"held":false}}}}]`
	b := parsePgBackRestInfo(out)
	if b.Count != 2 || b.LastAt != 1791471620 || b.Type != "incr" || b.Err != "" {
		t.Fatalf("%+v", b)
	}
	none := parsePgBackRestInfo(`[{"name":"pb","backup":[],"status":{"code":2,"message":"no valid backups"}}]`)
	if none.Count != 0 || none.Err != "" {
		t.Fatalf("no backups yet is not an error: %+v", none)
	}
	if bad := parsePgBackRestInfo("ERROR: [039]: HTTP request failed"); bad.Err == "" {
		t.Fatal("unparseable output is an error")
	}
}

func TestParsePBMList(t *testing.T) {
	out := `{"snapshots":[{"name":"2026-10-08T14:00:00Z","status":"done","restoreTo":1791468010,"type":"logical"},
	  {"name":"2026-10-08T15:00:00Z","status":"running","restoreTo":0}],"pitr":{"on":false}}`
	b := parsePBMList(out)
	if b.Count != 1 || b.LastAt != 1791468010 || !b.Running || b.Type != "logical" {
		t.Fatalf("%+v", b)
	}
}
