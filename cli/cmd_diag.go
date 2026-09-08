package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// cmd_diag.go — the "find out what happened" tools: Log Summary, FTDC Summary,
// Stalk Summary and the Packet Inspector.
//
// These four are the reason the CLI is worth having on a support engineer's machine
// rather than only in CI: the request that starts them is trivial, and the thing you
// want back is a file or a JSON document to attach to a ticket. So every one of them
// prints the server's own response (`--json` is the default shape here, because a
// verdict object has no sensible table form) and the ones that produce a file stream
// it straight to disk.

// ------------------------------------------------------------- Log Summary

func cmdLogs(args []string) error {
	return sub("logs", args, map[string]func([]string) error{
		"targets": logsTargets,
		"collect": logsCollect,
		"list":    logsList,
		"get":     logsGet,
		"events":  logsEvents,
		"delete":  logsDelete,
	}, []string{"targets", "collect", "list", "get", "events", "delete"})
}

func logsTargets(args []string) error {
	return simpleGet("logs targets", args, "/api/logsummary/targets")
}

func logsList(args []string) error {
	return simpleGet("logs list", args, "/api/logsummary/bundles")
}

func logsCollect(args []string) error {
	fs := flagsFor("logs collect")
	nodes := fs.String("nodes", "", "comma-separated node ids; omit for every node in the stack")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a stack name or id: dbcanvas logs collect <stack> [--nodes a,b]"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	st, err := resolveStack(c, arg(0))
	if err != nil {
		return err
	}
	body := map[string]any{"stackId": st.ID}
	if *nodes != "" {
		ids, err := resolveNodes(c, st.ID, *nodes)
		if err != nil {
			return err
		}
		body["nodeIds"] = strings.Split(ids, ",")
	}
	var raw []byte
	if err := c.request("POST", "/api/logsummary/collect", body, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func logsGet(args []string) error {
	fs := flagsFor("logs get")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a bundle id (see `dbcanvas logs list`)"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/logsummary/bundles/"+url.PathEscape(arg(0)), &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func logsEvents(args []string) error {
	fs := flagsFor("logs events")
	sev := fs.String("severity", "", "only this severity")
	limit := fs.Int("limit", 0, "how many events to return")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a bundle id (see `dbcanvas logs list`)"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	q := url.Values{}
	if *sev != "" {
		q.Set("severity", *sev)
	}
	if *limit > 0 {
		q.Set("limit", fmt.Sprint(*limit))
	}
	path := "/api/logsummary/bundles/" + url.PathEscape(arg(0)) + "/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var raw []byte
	if err := c.get(path, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func logsDelete(args []string) error {
	fs := flagsFor("logs delete")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a bundle id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	if err := c.delete("/api/logsummary/bundles/"+url.PathEscape(arg(0)), nil); err != nil {
		return err
	}
	fmt.Printf("Deleted log bundle %s.\n", arg(0))
	return nil
}

// ------------------------------------------------------------- FTDC Summary

func cmdFTDC(args []string) error {
	return sub("ftdc", args, map[string]func([]string) error{
		"targets": ftdcTargets,
		"node":    ftdcNode,
	}, []string{"targets", "node"})
}

func ftdcTargets(args []string) error {
	return simpleGet("ftdc targets", args, "/api/ftdc/targets")
}

func ftdcNode(args []string) error {
	fs := flagsFor("ftdc node")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a MongoDB node: dbcanvas ftdc node <stack> <node>"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.request("POST",
		fmt.Sprintf("/api/stacks/%d/nodes/%s/ftdc", st.ID, url.PathEscape(node)), nil, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

// ------------------------------------------------------------- Stalk Summary

func cmdStalk(args []string) error {
	return sub("stalk", args, map[string]func([]string) error{
		"start":    stalkStart,
		"status":   stalkStatus,
		"download": stalkDownload,
		"archives": stalkArchives,
		"analyse":  stalkAnalyse,
		"analyze":  stalkAnalyse, // both spellings; nobody should have to guess
	}, []string{"start", "status", "download", "archives", "analyse"})
}

func stalkStart(args []string) error {
	fs := flagsFor("stalk start")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas stalk start <stack> <node>"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	if err := c.post(fmt.Sprintf("/api/stacks/%d/nodes/%s/ptstalk",
		st.ID, url.PathEscape(node)), nil, nil); err != nil {
		return err
	}
	fmt.Printf("pt-stalk started on %s. Follow it with `dbcanvas stalk status %s %s`.\n",
		arg(1), arg(0), arg(1))
	return nil
}

func stalkStatus(args []string) error {
	fs := flagsFor("stalk status")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get(fmt.Sprintf("/api/stacks/%d/nodes/%s/ptstalk",
		st.ID, url.PathEscape(node)), &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func stalkDownload(args []string) error {
	fs := flagsFor("stalk download")
	out := fs.String("out", "", "where to save it (a directory or a file name)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	written, err := c.download(fmt.Sprintf("/api/stacks/%d/nodes/%s/ptstalk/download",
		st.ID, url.PathEscape(node)), *out)
	if err != nil {
		return err
	}
	fmt.Printf("Saved %s\n", written)
	return nil
}

func stalkArchives(args []string) error {
	return simpleGet("stalk archives", args, "/api/ptstalk/archives")
}

func stalkAnalyse(args []string) error {
	fs := flagsFor("stalk analyse")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a kept archive id (see `dbcanvas stalk archives`)"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.request("POST", "/api/stalksummary/archive/"+url.PathEscape(arg(0)), nil, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

// ------------------------------------------------------------- Packet Inspector

func cmdCapture(args []string) error {
	return sub("capture", args, map[string]func([]string) error{
		"targets":  captureTargets,
		"start":    captureStart,
		"list":     captureList,
		"get":      captureGet,
		"stop":     captureStop,
		"packets":  capturePackets,
		"download": captureDownload,
	}, []string{"targets", "start", "list", "get", "stop", "packets", "download"})
}

func captureTargets(args []string) error {
	return simpleGet("capture targets", args, "/api/pktinspect/targets")
}

func captureList(args []string) error {
	return simpleGet("capture list", args, "/api/pktinspect/captures")
}

func captureStart(args []string) error {
	fs := flagsFor("capture start")
	seconds := fs.Int("seconds", 30, "how long to capture for")
	label := fs.String("label", "", "a name for the capture")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas capture start <stack> <node> [--seconds N]"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	body := map[string]any{"stackId": st.ID, "nodeId": node, "seconds": *seconds}
	if *label != "" {
		body["label"] = *label
	}
	var raw []byte
	if err := c.request("POST", "/api/pktinspect/captures", body, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func captureGet(args []string) error {
	fs := flagsFor("capture get")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a capture id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/pktinspect/captures/"+url.PathEscape(arg(0)), &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func captureStop(args []string) error {
	fs := flagsFor("capture stop")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a capture id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.request("POST",
		"/api/pktinspect/captures/"+url.PathEscape(arg(0))+"/stop", nil, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func capturePackets(args []string) error {
	fs := flagsFor("capture packets")
	limit := fs.Int("limit", 0, "how many packets to return")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a capture id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	path := "/api/pktinspect/captures/" + url.PathEscape(arg(0)) + "/packets"
	if *limit > 0 {
		path += "?limit=" + fmt.Sprint(*limit)
	}
	var raw []byte
	if err := c.get(path, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func captureDownload(args []string) error {
	fs := flagsFor("capture download")
	out := fs.String("out", "", "where to save the .pcap")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a capture id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	written, err := c.download(
		"/api/pktinspect/captures/"+url.PathEscape(arg(0))+"/download", *out)
	if err != nil {
		return err
	}
	fmt.Printf("Saved %s — open it in Wireshark.\n", written)
	return nil
}

// ------------------------------------------------------------- shared helpers

// simpleGet is the whole implementation of a subcommand that takes no arguments and
// prints one endpoint's JSON. Written once because there are eight of them, and
// eight copies of six lines is eight places for the error handling to drift.
func simpleGet(name string, args []string, path string) error {
	fs := flagsFor(name)
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get(path, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

// stackAndClient resolves the client and a stack in one step, which most of these
// commands need before they do anything.
func stackAndClient(ref string) (*Client, Stack, error) {
	c, err := mustClient()
	if err != nil {
		return nil, Stack{}, err
	}
	st, err := resolveStack(c, ref)
	if err != nil {
		return nil, Stack{}, err
	}
	return c, st, nil
}

// ------------------------------------------------------------- dashboard & notifications

func cmdDashboard(args []string) error {
	fs := flagsFor("dashboard")
	live := fs.Bool("live", false, "include the live CPU/memory/network sample as well as the counters")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	path := "/api/dashboard/summary"
	if *live {
		path = "/api/dashboard/stats"
	}
	var raw []byte
	if err := c.get(path, &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func cmdNotifications(args []string) error {
	fs := flagsFor("notifications")
	readAll := fs.Bool("read-all", false, "mark every notification read instead of listing them")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	if *readAll {
		if err := c.post("/api/notifications/read-all", nil, nil); err != nil {
			return err
		}
		fmt.Println("Marked every notification read.")
		return nil
	}
	var raw []byte
	if err := c.get("/api/notifications", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var res struct {
		Unread int `json:"unread"`
		Items  []struct {
			Severity  string `json:"severity"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			CreatedAt string `json:"createdAt"`
			ReadAt    string `json:"readAt"`
		} `json:"items"`
	}
	if err := jsonUnmarshal(raw, &res); err != nil {
		return err
	}
	if len(res.Items) == 0 {
		empty("notifications", "")
		return nil
	}
	t := newTable("when", "severity", "title", "read")
	for _, n := range res.Items {
		t.add(shortTime(n.CreatedAt), n.Severity, truncate(n.Title, 48), yn(n.ReadAt != ""))
	}
	t.print()
	fmt.Fprintf(os.Stderr, "\n%d unread.\n", res.Unread)
	return nil
}
