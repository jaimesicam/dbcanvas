package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// cmd_doctor.go — `dbcanvas doctor`: everything worth checking in one place (doctor.go): Docker
// and its architecture, the images, the catalogues, the data folder, room for a stack network,
// k3d and the bundled files, and the service itself. It asks the running service, starting it;
// when the service cannot start, it runs the same checks itself so the cause still shows.

// errDoctorFailed is a doctor run with a failed check.
var errDoctorFailed = errors.New("a check failed")

func cliDoctor(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dbcanvas doctor")
	}
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	var checks []doctorCheck
	svcErr := ensureService(dir)
	if svcErr == nil {
		var out struct {
			Checks []doctorCheck `json:"checks"`
		}
		if err := (ctl{c: ctlClient(dir)}).do("GET", "/api/system/doctor", nil, &out); err != nil {
			return err
		}
		checks = out.Checks
		var st struct {
			PID     int    `json:"pid"`
			URL     string `json:"url"`
			Updated bool   `json:"binaryUpdated"`
		}
		(ctl{c: ctlClient(dir)}).do("GET", "/api/solo/status", nil, &st)
		svc := doctorCheck{Name: "service", Status: "ok", Detail: fmt.Sprintf("running (pid %d) at %s/", st.PID, st.URL)}
		if st.Updated {
			svc.Status, svc.Detail = "warn", svc.Detail+"; its binary was updated since it started"
			svc.Fix = "it restarts by itself once nothing is connected, or run dbcanvas stop"
		}
		checks = append([]doctorCheck{svc}, checks...)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a := &App{docker: NewDocker(dockerSocket())}
		applyBundle(dir)
		checks = append([]doctorCheck{{Name: "service", Status: "fail", Detail: svcErr.Error(), Fix: "see " + dir + "/" + soloLogName}},
			a.doctorChecks(ctx)...)
	}
	failed := false
	width := 0
	for _, c := range checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range checks {
		mark := map[string]string{"ok": "ok  ", "warn": "WARN", "fail": "FAIL"}[c.Status]
		fmt.Printf("%s  %-*s  %s\n", mark, width, c.Name, c.Detail)
		if c.Fix != "" && c.Status != "ok" {
			fmt.Printf("      %s  → %s\n", strings.Repeat(" ", width), c.Fix)
		}
		if c.Status == "fail" {
			failed = true
		}
	}
	if failed {
		return errDoctorFailed
	}
	return nil
}
