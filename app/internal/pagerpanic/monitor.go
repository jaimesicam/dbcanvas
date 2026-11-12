package pagerpanic

import (
	"fmt"
	"strconv"
	"strings"
)

// MonitorMaxBody is how much of a monitored page the game is given.
const MonitorMaxBody = 64 << 10

// MonitorView is a monitored page as the game's browser pane shows it.
type MonitorView struct {
	Title string `json:"title"`
	// URL is the address bar: the Service's cluster name, not the ClusterIP it was fetched at.
	URL    string `json:"url"`
	Status int    `json:"status"` // the HTTP status; 0 when nothing answered
	OK     bool   `json:"ok"`     // a 2xx or 3xx answer
	// Error is why nothing answered, in a browser's words; Detail is what curl or kubectl said.
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	Type   string `json:"type,omitempty"` // the Content-Type
	Body   string `json:"body,omitempty"`
}

// MonitorPlan is a monitor rendered for one instance: where to fetch it from, and how.
type MonitorPlan struct {
	Title string
	URL   string // as the address bar shows it
	On    string
	Env   []string // for MonitorScript
}

// MonitorScript fetches a monitored page. It prints a "PPMON <curl exit> <status>
// <content type>" line, then the page, or the error when nothing answered. The values
// come in the environment (PP_MON_*), so no var is ever pasted into the script.
const MonitorScript = `url="$PP_MON_URL"
if [ -n "$PP_MON_SVC" ]; then
  ns="${PP_MON_NS:-default}"
  if ! svc=$(kubectl -n "$ns" get svc "$PP_MON_SVC" -o jsonpath='{.spec.clusterIP} {.spec.ports[0].port}' 2>&1); then
    echo "PPMON nosvc 0 -"; echo "$svc" | head -c 2000; exit 0
  fi
  ip=${svc%% *}; port=${PP_MON_PORT:-${svc##* }}
  if [ -z "$ip" ] || [ "$ip" = None ]; then echo "PPMON noip 0 -"; echo "service $PP_MON_SVC has no ClusterIP"; exit 0; fi
  url="http://$ip:$port${PP_MON_PATH:-/}"
fi
f=$(mktemp); e=$(mktemp)
w=$(curl -sS -m 5 -o "$f" -w '%{http_code} %{content_type}' "$url" 2>"$e"); rc=$?
echo "PPMON $rc ${w:-000}"
if [ "$rc" = 0 ]; then head -c 65536 "$f"; else head -c 2000 "$e"; fi
rm -f "$f" "$e"
`

// MonitorPlan renders the task's monitor for an instance; ok is false when it has none.
func (t Task) MonitorPlan(inst Instance) (MonitorPlan, bool, error) {
	m := t.Monitor
	if m == nil {
		return MonitorPlan{}, false, nil
	}
	var err error
	r := func(s string) string {
		out, e := inst.Render(s)
		if e != nil && err == nil {
			err = e
		}
		return strings.TrimSpace(out)
	}
	p := MonitorPlan{Title: r(m.Title), On: m.On}
	if m.URL != "" {
		p.URL = r(m.URL)
		p.Env = []string{"PP_MON_URL=" + p.URL}
		return p, true, err
	}
	svc, ns, port, path := r(m.Service), r(m.Namespace), r(m.Port), r(m.Path)
	if ns == "" {
		ns = "default"
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	p.URL = "http://" + svc + "." + ns + ".svc"
	if port != "" && port != "80" {
		p.URL += ":" + port
	}
	p.URL += path
	p.Env = []string{"PP_MON_SVC=" + svc, "PP_MON_NS=" + ns, "PP_MON_PORT=" + port, "PP_MON_PATH=" + path}
	return p, true, err
}

// ParseMonitor reads what MonitorScript printed.
func ParseMonitor(p MonitorPlan, out string) MonitorView {
	v := MonitorView{Title: p.Title, URL: p.URL}
	head, body, _ := strings.Cut(out, "\n")
	f := strings.Fields(head)
	if len(f) < 3 || f[0] != "PPMON" {
		v.Error, v.Detail = "The page could not be fetched", Summarize(out)
		return v
	}
	if len(f) >= 4 {
		v.Type = f[3]
	}
	switch f[1] {
	case "nosvc":
		v.Error, v.Detail = "This site can't be reached", strings.TrimSpace(body)
		return v
	case "noip":
		v.Error, v.Detail = "This site can't be reached", strings.TrimSpace(body)
		return v
	case "0":
		v.Status, _ = strconv.Atoi(f[2])
		v.OK = v.Status >= 200 && v.Status < 400
		if len(body) > MonitorMaxBody {
			body = body[:MonitorMaxBody]
		}
		v.Body = body
		return v
	}
	rc, _ := strconv.Atoi(f[1])
	v.Error = curlError(rc)
	v.Detail = strings.TrimSpace(body)
	return v
}

// curlError puts a curl exit code the way a browser would.
func curlError(rc int) string {
	switch rc {
	case 6:
		return "Server not found (ERR_NAME_NOT_RESOLVED)"
	case 7:
		return "Connection refused (ERR_CONNECTION_REFUSED)"
	case 28:
		return "The connection timed out (ERR_CONNECTION_TIMED_OUT)"
	case 52:
		return "Empty response (ERR_EMPTY_RESPONSE)"
	case 56:
		return "The connection was reset (ERR_CONNECTION_RESET)"
	}
	return fmt.Sprintf("This site can't be reached (curl exit %d)", rc)
}
