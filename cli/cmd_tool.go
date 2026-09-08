package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// cmd_tool.go — the tools that run against a deployed stack.
//
// All three follow the same shape, because all three are long-running jobs the API
// starts and then reports on: POST to begin, poll for progress, and --wait to hold
// until it is finished. The polling is here rather than in the server because a job
// that outlives one HTTP request is the right design — the UI relies on exactly the
// same endpoints to draw its progress bars.

// ------------------------------------------------------------- Data Generator

func cmdDatagen(args []string) error {
	return sub("datagen", args, map[string]func([]string) error{
		"connections": datagenConnections,
		"databases":   datagenDatabases,
		"tables":      datagenTables,
		"run":         datagenRun,
	}, []string{"connections", "databases", "tables", "run"})
}

func datagenConnections(args []string) error {
	fs := flagsFor("datagen connections")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/datagen/connections", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var conns []struct {
		StackID   int64  `json:"stackId"`
		StackName string `json:"stackName"`
		NodeID    string `json:"nodeId"`
		Label     string `json:"label"`
		Engine    string `json:"engine"`
	}
	if err := jsonUnmarshal(raw, &conns); err != nil {
		return err
	}
	if len(conns) == 0 {
		empty("nodes the Data Generator can write to", "Deploy a database stack first.")
		return nil
	}
	t := newTable("stack", "node", "engine")
	for _, cn := range conns {
		label := cn.Label
		if label == "" {
			label = cn.NodeID
		}
		t.add(cn.StackName, label, cn.Engine)
	}
	t.print()
	return nil
}

func datagenDatabases(args []string) error {
	return datagenIntrospect("databases", args)
}

func datagenTables(args []string) error {
	return datagenIntrospect("tables", args)
}

// datagenIntrospect covers `databases` and `tables`, which differ only in the path
// and in whether --database is required.
func datagenIntrospect(what string, args []string) error {
	fs := flagsFor("datagen " + what)
	db := fs.String("database", "", "which database to look in")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas datagen "+what+" <stack> <node>"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/datagen/stacks/%d/nodes/%s/%s", st.ID, url.PathEscape(node), what)
	if *db != "" {
		path += "?database=" + url.QueryEscape(*db)
	} else if what == "tables" {
		return fmt.Errorf("--database is required to list tables")
	}
	var raw []byte
	if err := c.get(path, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func datagenRun(args []string) error {
	fs := flagsFor("datagen run")
	db := fs.String("database", "", "which database to write into")
	table := fs.String("table", "", "which table or collection to fill")
	rows := fs.Int("rows", 0, "how many rows to generate")
	wait := fs.Bool("wait", false, "wait for the job to finish")
	timeout := fs.Duration("timeout", 2*time.Hour, "how long --wait will wait")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas datagen run <stack> <node> --table T --rows N"); err != nil {
		return err
	}
	if *table == "" || *rows <= 0 {
		fmt.Fprintln(os.Stderr, "dbcanvas: --table and --rows are both required")
		return errUsage
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	body := map[string]any{"table": *table, "rows": *rows}
	if *db != "" {
		body["database"] = *db
	}
	var started struct {
		JobID string `json:"jobId"`
		ID    string `json:"id"`
	}
	if err := c.post(fmt.Sprintf("/api/datagen/stacks/%d/nodes/%s/generate",
		st.ID, url.PathEscape(node)), body, &started); err != nil {
		return err
	}
	job := started.JobID
	if job == "" {
		job = started.ID
	}
	if job == "" {
		return fmt.Errorf("the server started a job but returned no id")
	}
	fmt.Printf("Generating %d rows into %s (job %s).\n", *rows, *table, job)
	if !*wait {
		fmt.Printf("  Follow it with `dbcanvas api GET /api/datagen/jobs/%s`.\n", job)
		return nil
	}
	return waitForJob(c, "/api/datagen/jobs/"+url.PathEscape(job), *timeout)
}

// waitForJob polls a job endpoint that reports {status, progress, error} until it
// stops running.
func waitForJob(c *Client, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var job struct {
			Status   string  `json:"status"`
			Progress float64 `json:"progress"`
			Written  int64   `json:"written"`
			Rows     int64   `json:"rows"`
			Error    string  `json:"error"`
		}
		if err := c.get(path, &job); err != nil {
			return err
		}
		line := fmt.Sprintf("  %s", job.Status)
		if job.Rows > 0 {
			line = fmt.Sprintf("  %s  %d/%d rows", job.Status, job.Written, job.Rows)
		} else if job.Progress > 0 {
			line = fmt.Sprintf("  %s  %.0f%%", job.Status, job.Progress*100)
		}
		if line != last {
			fmt.Println(line)
			last = line
		}
		switch job.Status {
		case "done", "complete", "finished":
			return nil
		case "error", "failed":
			msg := job.Error
			if msg == "" {
				msg = "the job failed"
			}
			return fmt.Errorf("%w: %s", errWaitFailed, msg)
		case "cancelled", "canceled":
			return fmt.Errorf("%w: the job was cancelled", errWaitFailed)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: still %s after %s", errWaitFailed, job.Status, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// ------------------------------------------------------------- Query Runner

func cmdQuery(args []string) error {
	return sub("query", args, map[string]func([]string) error{
		"targets": queryTargets,
		"run":     queryRun,
		"history": queryHistory,
	}, []string{"targets", "run", "history"})
}

func queryTargets(args []string) error {
	fs := flagsFor("query targets")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/queryrun/targets", &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func queryRun(args []string) error {
	fs := flagsFor("query run")
	stack := fs.String("stack", "", "the stack the nodes belong to")
	nodes := fs.String("nodes", "", "comma-separated node ids to run against")
	sql := fs.String("sql", "", "the statement to run, or @file")
	wait := fs.Bool("wait", true, "wait for the run to finish (default true)")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long --wait will wait")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *stack == "" || *nodes == "" || *sql == "" {
		fmt.Fprintln(os.Stderr, "dbcanvas: --stack, --nodes and --sql are all required")
		return errUsage
	}
	statement := *sql
	if strings.HasPrefix(statement, "@") {
		raw, err := readFileOrStdin(strings.TrimPrefix(statement, "@"))
		if err != nil {
			return err
		}
		statement = string(raw)
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	st, err := resolveStack(c, *stack)
	if err != nil {
		return err
	}
	var started struct {
		ID string `json:"id"`
	}
	ids, err := resolveNodes(c, st.ID, *nodes)
	if err != nil {
		return err
	}
	if err := c.post("/api/queryrun/runs", map[string]any{
		"stackId": st.ID,
		"nodeIds": strings.Split(ids, ","),
		"sql":     statement,
	}, &started); err != nil {
		return err
	}
	if started.ID == "" {
		return fmt.Errorf("the server started a run but returned no id")
	}
	if !*wait {
		fmt.Printf("Started run %s.\n", started.ID)
		return nil
	}
	deadline := time.Now().Add(*timeout)
	for {
		var raw []byte
		if err := c.get("/api/queryrun/runs/"+url.PathEscape(started.ID), &raw); err != nil {
			return err
		}
		var run struct {
			Status string `json:"status"`
		}
		jsonUnmarshal(raw, &run)
		if run.Status != "running" && run.Status != "" {
			return printRaw(raw)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: run %s is still going after %s", errWaitFailed, started.ID, *timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func queryHistory(args []string) error {
	fs := flagsFor("query history")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/queryrun/history", &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

// ------------------------------------------------------------- Benchmark

func cmdBenchmark(args []string) error {
	return sub("benchmark", args, map[string]func([]string) error{
		"targets": benchTargets,
		"run":     benchRun,
		"history": benchHistory,
	}, []string{"targets", "run", "history"})
}

func benchTargets(args []string) error {
	fs := flagsFor("benchmark targets")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/benchmark/targets", &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func benchRun(args []string) error {
	fs := flagsFor("benchmark run")
	workload := fs.String("workload", "oltp", "oltp, olap, rw or ro")
	duration := fs.Int("duration", 60, "how long to run, in seconds")
	threads := fs.Int("threads", 0, "client threads (0 = the server's default)")
	wait := fs.Bool("wait", false, "wait for the run to finish and print the numbers")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas benchmark run <stack> <node>"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	body := map[string]any{
		"stackId":  st.ID,
		"nodeId":   node,
		"workload": *workload,
		"duration": *duration,
	}
	if *threads > 0 {
		body["threads"] = *threads
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := c.post("/api/benchmark/runs", body, &started); err != nil {
		return err
	}
	if started.ID == "" {
		return fmt.Errorf("the server started a benchmark but returned no id")
	}
	fmt.Printf("Running the %s workload on %s for %ds (run %s).\n",
		*workload, arg(1), *duration, started.ID)
	if !*wait {
		fmt.Printf("  Follow it with `dbcanvas api GET /api/benchmark/runs/%s`.\n", started.ID)
		return nil
	}
	// The duration is known, so wait a little past it rather than polling blindly.
	deadline := time.Now().Add(time.Duration(*duration)*time.Second + 10*time.Minute)
	for {
		var raw []byte
		if err := c.get("/api/benchmark/runs/"+url.PathEscape(started.ID), &raw); err != nil {
			return err
		}
		var run struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		jsonUnmarshal(raw, &run)
		switch run.Status {
		case "done", "complete", "finished":
			return printRaw(raw)
		case "error", "failed":
			msg := run.Error
			if msg == "" {
				msg = "the benchmark failed"
			}
			return fmt.Errorf("%w: %s", errWaitFailed, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: benchmark %s never finished", errWaitFailed, started.ID)
		}
		time.Sleep(5 * time.Second)
	}
}

func benchHistory(args []string) error {
	fs := flagsFor("benchmark history")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/benchmark/history", &raw); err != nil {
		return err
	}
	return printRaw(raw)
}
