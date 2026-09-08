// haproxy.go fetches and parses this app's linked HAProxy node's own CSV
// stats page — a plain http.Get from inside the container (MarketChaos is
// on the same stack network as everything else, no docker-exec plumbing
// needed, unlike the Labs helpers which run from the dbcanvas orchestrator
// itself). Only relevant when TARGET_KIND is "haproxy-pxc"/"haproxy-mysql"
// (HAPROXY_STATS_URL set — see app/marketchaos.go); every other target
// shape never calls this.
package store

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HAProxyRow is one server (or BACKEND aggregate) row from the stats CSV —
// only the columns the HAProxy panel actually shows.
type HAProxyRow struct {
	Backend string // pxname, e.g. "mysql-write", "mysql-read"
	Server  string // svname, e.g. "pxc-1", "BACKEND"
	Status  string // "UP", "DOWN", "no check", "UP (backup)" ...
	Weight  int
	CurSess int
	TotSess int64
}

// FetchHAProxyStats fetches statsURL+";csv" and parses it into rows,
// skipping the synthetic "FRONTEND" rows this panel doesn't care about.
func FetchHAProxyStats(ctx context.Context, statsURL string) ([]HAProxyRow, error) {
	url := strings.TrimRight(statsURL, "/") + "/;csv"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("haproxy stats: HTTP %d", resp.StatusCode)
	}

	r := csv.NewReader(resp.Body)
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil || len(records) == 0 {
		return nil, fmt.Errorf("haproxy stats: %w", err)
	}

	// Header row starts with "# pxname" — column order is stable within a
	// version but not guaranteed across HAProxy versions, so look columns
	// up by name rather than fixed index.
	header := records[0]
	header[0] = strings.TrimPrefix(header[0], "# ")
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	idx := func(name string) int { return col[name] }

	var out []HAProxyRow
	for _, rec := range records[1:] {
		svname := field(rec, idx("svname"))
		if svname == "FRONTEND" {
			continue
		}
		out = append(out, HAProxyRow{
			Backend: field(rec, idx("pxname")),
			Server:  svname,
			Status:  field(rec, idx("status")),
			Weight:  atoiField(rec, idx("weight")),
			CurSess: atoiField(rec, idx("scur")),
			TotSess: int64(atoiField(rec, idx("stot"))),
		})
	}
	return out, nil
}

func field(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i]
}

func atoiField(rec []string, i int) int {
	n, _ := strconv.Atoi(field(rec, i))
	return n
}
