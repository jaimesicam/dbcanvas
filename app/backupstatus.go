package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// backupstatus.go — when a cluster was last backed up, for the badge on its header: the question
// every DBA asks of a cluster before touching it, and the one a lab forgets — backups configured
// at deploy and never taken look exactly like backups that work.
//
// It is asked of the backup tool itself, from a member, about the repository in the object store:
// pgBackRest's `info` (Patroni, repmgr with pgBackRest, standalone PostgreSQL) and PBM's `list`
// (MongoDB). Barman cloud is not read (its listing needs the store's credentials on the command
// line); the badge says so rather than guessing.

type backupInfo struct {
	Engine  string `json:"engine"`           // pgbackrest | pbm | barman
	Count   int    `json:"count"`            // backups in the repository
	LastAt  int64  `json:"lastAt,omitempty"` // unix seconds the newest one finished
	Last    string `json:"last,omitempty"`   // its label or name
	Type    string `json:"type,omitempty"`   // full / incr / diff / logical / physical
	Size    int64  `json:"size,omitempty"`   // bytes, as the tool reports them
	Running bool   `json:"running,omitempty"`
	Err     string `json:"error,omitempty"`
}

// pgBackRest `info --output=json`: one entry per stanza.
type pgbrInfo struct {
	Name   string `json:"name"`
	Backup []struct {
		Label     string `json:"label"`
		Type      string `json:"type"`
		Timestamp struct {
			Start int64 `json:"start"`
			Stop  int64 `json:"stop"`
		} `json:"timestamp"`
		Info struct {
			Size int64 `json:"size"`
		} `json:"info"`
	} `json:"backup"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Lock    struct {
			Backup struct {
				Held bool `json:"held"`
			} `json:"backup"`
		} `json:"lock"`
	} `json:"status"`
}

func parsePgBackRestInfo(out string) backupInfo {
	b := backupInfo{Engine: "pgbackrest"}
	var stanzas []pgbrInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &stanzas); err != nil || len(stanzas) == 0 {
		b.Err = clipLine(firstNonEmpty(strings.TrimSpace(out), "pgbackrest info returned nothing"), 200)
		return b
	}
	s := stanzas[0]
	b.Count = len(s.Backup)
	b.Running = s.Status.Lock.Backup.Held
	if s.Status.Code != 0 && s.Status.Code != 2 { // 2: no backups yet
		b.Err = s.Status.Message
	}
	for _, x := range s.Backup {
		if x.Timestamp.Stop >= b.LastAt {
			b.LastAt, b.Last, b.Type, b.Size = x.Timestamp.Stop, x.Label, x.Type, x.Info.Size
		}
	}
	return b
}

// PBM `list --out=json`: its snapshots, each with the time it restores to.
func parsePBMList(out string) backupInfo {
	b := backupInfo{Engine: "pbm"}
	var l struct {
		Snapshots []struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			RestoreTo int64  `json:"restoreTo"`
			Type      string `json:"type"`
			Size      int64  `json:"size"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &l); err != nil {
		b.Err = clipLine(firstNonEmpty(strings.TrimSpace(out), "pbm list returned nothing"), 200)
		return b
	}
	sort.Slice(l.Snapshots, func(i, j int) bool { return l.Snapshots[i].RestoreTo < l.Snapshots[j].RestoreTo })
	for _, s := range l.Snapshots {
		if s.Status != "" && s.Status != "done" {
			if s.Status == "running" || s.Status == "starting" {
				b.Running = true
			}
			continue
		}
		b.Count++
		b.LastAt, b.Last, b.Type, b.Size = s.RestoreTo, s.Name, s.Type, s.Size
	}
	return b
}

// handleBackupStatus is GET /api/stacks/{id}/frames/{fid}/backup-status.
func (a *App) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	fid := r.PathValue("fid")
	doc := buildDoc(st)
	var frame designFrame
	for _, f := range doc.Frames {
		if f.ID == fid {
			frame = f
		}
	}
	if frame.ID == "" {
		writeErr(w, http.StatusNotFound, "no such cluster")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, a.frameBackupStatus(ctx, st, doc, frame))
}

func (a *App) frameBackupStatus(ctx context.Context, st Stack, doc designDoc, f designFrame) backupInfo {
	switch {
	case f.Type == "patroni" && f.UsePgBackRest:
		cid := a.patroniLeaderContainer(ctx, st, f, doc)
		return a.pgbrInfoOn(ctx, st, f, cid, patroniStanza(f.Label))
	case f.Type == "repmgr" && repmgrBackupEngine(f) == "pgbackrest":
		cid := a.repmgrPrimaryContainer(ctx, st, f, doc)
		return a.pgbrInfoOn(ctx, st, f, cid, repmgrStanza(f.Label))
	case f.Type == "repmgr" && repmgrBackupEngine(f) == "barman":
		return backupInfo{Engine: "barman", Err: "Barman cloud backups are not listed here — see the cluster's Backup tab"}
	case (f.Type == "psmrs" || f.Type == "psmdb") && f.EnablePBM:
		for _, n := range doc.Nodes {
			if n.FrameID != f.ID || n.Role == "mongos" || (n.Type != "psmrs" && n.Type != "psmdb") {
				continue
			}
			dep, err := a.store.GetDeployment(st.ID, n.ID)
			if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
				continue
			}
			var sec mongoSecrets
			json.Unmarshal(dep.Secrets, &sec)
			c := withEngine(ctx, a.depEngine(st, n.ID))
			res, err := a.engCtx(c).Exec(c, dep.ContainerID, []string{"bash", "-c", `PBM_MONGODB_URI="$PBM_URI" pbm list --out=json 2>&1`},
				[]string{"PBM_URI=" + pbmMongoURI(sec.PBMUser, sec.PBMPassword)})
			if err != nil {
				return backupInfo{Engine: "pbm", Err: err.Error()}
			}
			return parsePBMList(res.Stdout)
		}
		return backupInfo{Engine: "pbm", Err: "no member is running"}
	}
	return backupInfo{}
}

func (a *App) pgbrInfoOn(ctx context.Context, st Stack, f designFrame, cid, stanza string) backupInfo {
	if cid == "" {
		return backupInfo{Engine: "pgbackrest", Err: "no running primary to ask"}
	}
	c := withEngine(ctx, a.depEngineForContainer(st, f, cid))
	res, err := a.engCtx(c).Exec(c, cid, []string{"bash", "-c", `runuser -u postgres -- pgbackrest --stanza="$STANZA" --output=json info 2>&1`}, []string{"STANZA=" + stanza})
	if err != nil {
		return backupInfo{Engine: "pgbackrest", Err: err.Error()}
	}
	b := parsePgBackRestInfo(res.Stdout)
	if b.Err == "" && res.Code != 0 {
		b.Err = fmt.Sprintf("pgbackrest info exited %d", res.Code)
	}
	return b
}

// depEngineForContainer is the engine of the frame member running a container.
func (a *App) depEngineForContainer(st Stack, f designFrame, cid string) Engine {
	return a.nodeEngine(st, f.Type)
}
