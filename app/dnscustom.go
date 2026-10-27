package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// dnscustom.go — DNS records added by hand on the Intranet's DNS tab: forward (A, CNAME) and
// reverse (PTR) entries beside the ones DBCanvas publishes for every node.
//
// The zone files are rebuilt from scratch on every reconcile (dns.go), so a record edited into
// the file inside the container would last until the next deploy. These are kept by DBCanvas
// instead — in app_settings under "dns:<stack>", per stack rather than in the design, because
// the designer saves the design whole from the browser and would overwrite a record added here
// since it loaded — and merged in by buildDNSZones every time the zones are written.
//
// A custom record wins over an automatic one: an A or CNAME replaces the automatic records of
// the same name, a PTR those of the same address. That is what most of them are for — point pmm
// at something else, give a node a second name, make a reverse lookup return a chosen host name —
// and it means a node added later cannot take a name back.

// dnsCustomRecord is one record added by hand.
//
//	A      Name: host relative to the domain (may be dotted, may start "*."); Value: IPv4.
//	       PTR publishes the matching reverse record too.
//	CNAME  Name as A; Value: the target, fully qualified (a bare host is taken as in-domain).
//	PTR    Name: IPv4; Value: the host name it returns, fully qualified the same way.
type dnsCustomRecord struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	PTR   bool   `json:"ptr,omitempty"`
}

func dnsCustomKey(stackID int64) string { return fmt.Sprintf("dns:%d", stackID) }

// dnsCustomMu serialises the read-modify-write of a stack's records.
var dnsCustomMu sync.Mutex

func (a *App) dnsCustomRecords(stackID int64) []dnsCustomRecord {
	v, _ := a.store.AppSetting(dnsCustomKey(stackID))
	var recs []dnsCustomRecord
	if v != "" {
		json.Unmarshal([]byte(v), &recs)
	}
	return recs
}

func (a *App) saveDNSCustomRecords(stackID int64, recs []dnsCustomRecord) error {
	if len(recs) == 0 {
		return a.store.DeleteAppSetting(dnsCustomKey(stackID))
	}
	b, _ := json.Marshal(recs)
	return a.store.SetAppSetting(dnsCustomKey(stackID), string(b))
}

// ---------------------------------------------------------------- validation

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// dnsLabels checks a dotted name label by label; a leading "*" label is allowed when wildcard is.
func dnsLabels(s string, wildcard bool) error {
	if s == "" {
		return fmt.Errorf("name is empty")
	}
	if len(s) > 253 {
		return fmt.Errorf("name is longer than 253 characters")
	}
	for i, l := range strings.Split(s, ".") {
		if l == "*" && i == 0 && wildcard {
			continue
		}
		if !dnsLabelRe.MatchString(l) {
			return fmt.Errorf("%q is not a valid DNS label in %q", l, s)
		}
	}
	return nil
}

// dnsRelName turns what was typed for an A or CNAME name into a name relative to the domain:
// "pmm", "pmm.<domain>" and "pmm.<domain>." all give "pmm".
func dnsRelName(s, domain string) (string, error) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if s == domain || s == "@" {
		return "", fmt.Errorf("the domain itself (%s) cannot be changed here", domain)
	}
	s = strings.TrimSuffix(s, "."+domain)
	if err := dnsLabels(s, true); err != nil {
		return "", err
	}
	if s == "intranet" {
		return "", fmt.Errorf("intranet is the stack's name server and cannot be redefined")
	}
	return s, nil
}

// dnsTarget turns a CNAME target or PTR host into a fully qualified name (no trailing dot): a bare
// host is taken as one in the domain, anything with a dot as fully qualified already.
func dnsTarget(s, domain string) (string, error) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if s != "" && !strings.Contains(s, ".") {
		s = fqdnOf(s, domain)
	}
	if err := dnsLabels(s, false); err != nil {
		return "", err
	}
	return s, nil
}

func dnsIPv4(s string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("%q is not an IPv4 address", strings.TrimSpace(s))
	}
	return ip.To4().String(), nil
}

// normalizeDNSRecord validates a record as typed and returns it in its stored form.
func normalizeDNSRecord(r dnsCustomRecord, domain string) (dnsCustomRecord, error) {
	var err error
	r.Type = strings.ToUpper(strings.TrimSpace(r.Type))
	switch r.Type {
	case "A":
		if r.Name, err = dnsRelName(r.Name, domain); err != nil {
			return r, err
		}
		if r.Value, err = dnsIPv4(r.Value); err != nil {
			return r, err
		}
		if r.PTR && strings.HasPrefix(r.Name, "*") {
			return r, fmt.Errorf("a wildcard name has no reverse record to publish")
		}
	case "CNAME":
		if r.Name, err = dnsRelName(r.Name, domain); err != nil {
			return r, err
		}
		if r.Value, err = dnsTarget(r.Value, domain); err != nil {
			return r, err
		}
		if r.Value == fqdnOf(r.Name, domain) {
			return r, fmt.Errorf("a CNAME cannot point at itself")
		}
		r.PTR = false
	case "PTR":
		if r.Name, err = dnsIPv4(r.Name); err != nil {
			return r, err
		}
		if r.Value, err = dnsTarget(r.Value, domain); err != nil {
			return r, err
		}
		r.PTR = false
	default:
		return r, fmt.Errorf("type must be A, CNAME or PTR")
	}
	return r, nil
}

// dnsConflict reports why r cannot sit beside the other records, "" when it can. A CNAME may not
// share its name with anything (RFC 1034 §3.6.2); exact duplicates are refused as typos.
func dnsConflict(r dnsCustomRecord, others []dnsCustomRecord) string {
	for _, o := range others {
		if o.ID == r.ID {
			continue
		}
		if o.Type == r.Type && o.Name == r.Name && o.Value == r.Value {
			return fmt.Sprintf("%s %s → %s already exists", r.Type, r.Name, r.Value)
		}
		if o.Type == "PTR" || r.Type == "PTR" || o.Name != r.Name {
			continue
		}
		if o.Type == "CNAME" || r.Type == "CNAME" {
			return fmt.Sprintf("%s already has a record; a CNAME cannot share its name with another record", r.Name)
		}
	}
	return ""
}

// ---------------------------------------------------------------- handlers

// dnsView is the DNS tab: the automatic records (with whether a custom one replaces them), the
// custom ones, the zones served, and whether the Intranet is running to serve them.
type dnsView struct {
	Domain       string            `json:"domain"`
	Applied      bool              `json:"applied"`
	ReverseZones []string          `json:"reverseZones"`
	Automatic    []dnsAutoRecord   `json:"automatic"`
	Custom       []dnsCustomRecord `json:"custom"`
}

type dnsAutoRecord struct {
	Type       string `json:"type"` // A | PTR
	Name       string `json:"name"`
	Value      string `json:"value"`
	Overridden bool   `json:"overridden"`
}

// loadIntranetNode resolves an owned stack whose {nid} is its Intranet node.
func (a *App) loadIntranetNode(w http.ResponseWriter, r *http.Request) (Stack, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, false
	}
	if nodeTypeOf(st, r.PathValue("nid")) != "intranet" {
		writeErr(w, http.StatusNotFound, "not an Intranet node")
		return Stack{}, false
	}
	return st, true
}

func (a *App) dnsViewOf(r *http.Request, st Stack) dnsView {
	custom := a.dnsCustomRecords(st.ID)
	v := dnsView{Domain: envOr("DOMAIN", "example.net"), Custom: custom,
		ReverseZones: []string{}, Automatic: []dnsAutoRecord{}}
	if v.Custom == nil {
		v.Custom = []dnsCustomRecord{}
	}
	p, ok := a.stackDNSPlan(r.Context(), st)
	if !ok {
		return v
	}
	v.Applied = true
	named, ptrs := map[string]bool{}, map[string]bool{}
	for _, c := range custom {
		switch {
		case c.Type == "PTR":
			ptrs[c.Name] = true
		default:
			named[c.Name] = true
			if c.Type == "A" && c.PTR {
				ptrs[c.Value] = true
			}
		}
	}
	for _, rec := range p.auto {
		v.Automatic = append(v.Automatic,
			dnsAutoRecord{Type: "A", Name: rec.host, Value: rec.ip, Overridden: named[rec.host]},
			dnsAutoRecord{Type: "PTR", Name: rec.ip, Value: fqdnOf(rec.host, p.domain), Overridden: ptrs[rec.ip]})
	}
	for _, z := range buildDNSZones(p.domain, 0, p.subnet, p.auto, custom).Reverse {
		v.ReverseZones = append(v.ReverseZones, z.Zone)
	}
	return v
}

func (a *App) handleDNSList(w http.ResponseWriter, r *http.Request) {
	st, ok := a.loadIntranetNode(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.dnsViewOf(r, st))
}

// dnsMutate adds, updates or deletes one custom record, rewrites the zones, and answers with the
// tab's new view. A record saved while the Intranet is down is applied on its next deploy.
func (a *App) dnsMutate(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, ok := a.loadIntranetNode(w, r)
		if !ok {
			return
		}
		var b dnsCustomRecord
		if err := decode(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		domain := envOr("DOMAIN", "example.net")

		dnsCustomMu.Lock()
		recs := a.dnsCustomRecords(st.ID)
		idx := -1
		for i, rec := range recs {
			if rec.ID == b.ID {
				idx = i
			}
		}
		if op != "add" && (b.ID == "" || idx < 0) {
			dnsCustomMu.Unlock()
			writeErr(w, http.StatusNotFound, "no such record")
			return
		}
		if op == "delete" {
			recs = append(recs[:idx], recs[idx+1:]...)
		} else {
			rec, err := normalizeDNSRecord(b, domain)
			if err == nil {
				if op == "add" {
					rec.ID = randHex(6)
				}
				if msg := dnsConflict(rec, recs); msg != "" {
					err = fmt.Errorf("%s", msg)
				}
			}
			if err != nil {
				dnsCustomMu.Unlock()
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if op == "add" {
				recs = append(recs, rec)
			} else {
				recs[idx] = rec
			}
		}
		err := a.saveDNSCustomRecords(st.ID, recs)
		dnsCustomMu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to save the record")
			return
		}

		if _, err := a.applyStackDNS(r.Context(), st.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, "saved, but the zones were not reloaded: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a.dnsViewOf(r, st))
	}
}
