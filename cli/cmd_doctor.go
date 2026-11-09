package main

import (
	"fmt"
	"strings"
)

// cmd_doctor.go — `dbcanvas-cli doctor`: the installation's checks (GET /api/system/doctor,
// administrators). Exits non-zero when a check fails, so a script can gate on it.

func cmdDoctor(args []string) error {
	fs := flagsFor("doctor")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var res struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Checks  []struct {
			Name, Status, Detail, Fix string
		} `json:"checks"`
	}
	if err := c.get("/api/system/doctor", &res); err != nil {
		return err
	}
	if g.json {
		if err := printJSON(res); err != nil {
			return err
		}
	} else {
		mark := map[string]string{"ok": "ok  ", "warn": "WARN", "fail": "FAIL"}
		for _, ch := range res.Checks {
			fmt.Printf("%s  %-18s %s\n", mark[ch.Status], ch.Name, ch.Detail)
			if ch.Fix != "" && ch.Status != "ok" {
				fmt.Printf("      %-18s → %s\n", "", ch.Fix)
			}
		}
		fmt.Printf("\nDBCanvas %s: %s\n", res.Version, strings.ToUpper(res.Status))
	}
	if res.Status == "fail" {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}
