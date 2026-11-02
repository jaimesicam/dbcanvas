package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// mysqltuning.go — InnoDB sizing and durability set on a MySQL-family server at deploy:
// innodb_buffer_pool_size, the redo log size, sync_binlog and innodb_flush_log_at_trx_commit.
//
// A cluster sets them on its frame for every member; a standalone Percona Server, MySQL or
// MariaDB node sets its own, and they ride on the synthetic frame its provisioner builds. Either
// way they go into the [mysqld] section each renderer writes (pxcMyCnf, mysqlMyCnf, innodbMyCnf,
// mariadbReplCnf, mariadbGaleraCnf). A value left empty writes nothing, so the server keeps its
// own default.
//
// The redo log is one option with two spellings. MySQL 8.0.30 replaced innodb_log_file_size and
// innodb_log_files_in_group with innodb_redo_log_capacity, the total; an older server refuses to
// start on the new name. So innodb_redo_log_capacity is written on 8.0.30 and later, and
// innodb_log_file_size before that (5.7, and a pinned 8.0 minor below .30), where it is the size
// of each of the two redo files. MariaDB has a single redo file since 10.5 and still calls it
// innodb_log_file_size, which there is the total.

// mysqlTuning is the four settings, embedded in designNode and designFrame so the JSON fields
// are the same on both. "" leaves the server default.
type mysqlTuning struct {
	InnoDBBufferPoolSize      cnfValue `json:"innodbBufferPoolSize"`      // e.g. "2G"
	InnoDBRedoLogCapacity     cnfValue `json:"innodbRedoLogCapacity"`     // e.g. "1G"; see mysqlRedoOption
	SyncBinlog                cnfValue `json:"syncBinlog"`                // "0" | "1" | N
	InnoDBFlushLogAtTrxCommit cnfValue `json:"innodbFlushLogAtTrxCommit"` // "0" | "1" | "2"
}

// cnfValue is an option value as the design carries it: a string, or a bare JSON number from an
// API client that sends sync_binlog as 1. "" means unset.
type cnfValue string

func (v *cnfValue) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] != '"' && string(b) != "null" {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*v = cnfValue(n.String())
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*v = cnfValue(s)
	return nil
}

func (v cnfValue) str() string { return strings.TrimSpace(string(v)) }

// mysqlTuningSize is a byte size as an option file takes it: digits with an optional K, M or G.
var mysqlTuningSize = regexp.MustCompile(`^[0-9]+[KMGkmg]?$`)

// mysqlSizeBytes is a mysqlTuningSize value in bytes, or -1 when it is not one.
func mysqlSizeBytes(s string) int64 {
	if !mysqlTuningSize.MatchString(s) {
		return -1
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n * mult
}

// mysqlFrameSeries is the server series and pinned minor a MySQL-family frame installs; the minor
// is "" when the frame takes the newest of its series.
func mysqlFrameSeries(f designFrame) (major, version string) {
	switch f.Type {
	case "pxc":
		return f.PXCMajor, f.PXCVersion
	case "innodb":
		// The repository fixes the version: pdps-8.0.19 pins a minor, pdps-84-lts does not.
		v := strings.TrimPrefix(strings.TrimPrefix(f.PDPSRepo, "pdps"), "-")
		if strings.Count(v, ".") < 2 {
			v = ""
		}
		return psMajorOfRepo(f.PDPSRepo), v
	case "mysqlcerepl", "mysqlceinnodb":
		return mysqlceMajorOf(f.MySQLCEMajor), f.MySQLCEVersion
	}
	return f.PSMajor, f.PSVersion
}

// mysqlHasRedoCapacity says whether a series and minor know innodb_redo_log_capacity (8.0.30+).
func mysqlHasRedoCapacity(major, version string) bool {
	switch major {
	case "5.7":
		return false
	case "8.0", "":
	default:
		return true
	}
	return mysqlPatch(version) >= 30
}

// mysqlPatch is the patch number of a version such as "8.0.45-36.1" or "8.0.19", or 99 when there
// is none (the newest of the series).
func mysqlPatch(version string) int {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) != 3 {
		return 99
	}
	p := parts[2]
	if i := strings.IndexAny(p, "-."); i >= 0 {
		p = p[:i]
	}
	if v, err := strconv.Atoi(p); err == nil {
		return v
	}
	return 99
}

// mysqlRedoOption is the option name the frame's redo log size is written under.
func mysqlRedoOption(f designFrame) string {
	if f.Type == "mariadbrepl" || f.Type == "mariadbgalera" {
		return "innodb_log_file_size"
	}
	if mysqlHasRedoCapacity(mysqlFrameSeries(f)) {
		return "innodb_redo_log_capacity"
	}
	return "innodb_log_file_size"
}

// mysqlTuningCnf renders the frame's settings as [mysqld] lines; "" when none is set.
func mysqlTuningCnf(f designFrame) string {
	var b strings.Builder
	if v := f.InnoDBBufferPoolSize.str(); v != "" {
		fmt.Fprintf(&b, "innodb_buffer_pool_size=%s\n", v)
	}
	if v := f.InnoDBRedoLogCapacity.str(); v != "" {
		fmt.Fprintf(&b, "%s=%s\n", mysqlRedoOption(f), v)
	}
	if v := f.SyncBinlog.str(); v != "" {
		fmt.Fprintf(&b, "sync_binlog=%s\n", v)
	}
	if v := f.InnoDBFlushLogAtTrxCommit.str(); v != "" {
		fmt.Fprintf(&b, "innodb_flush_log_at_trx_commit=%s\n", v)
	}
	return b.String()
}

// mysqlTuningFrameTypes are the frames that offer the settings.
var mysqlTuningFrameTypes = map[string]bool{
	"pxc": true, "mysql": true, "innodb": true, "mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true,
}

// mysqlTuningNodeTypes are the standalone servers that offer them, with the frame type their
// provisioner builds (which picks the redo option's name).
var mysqlTuningNodeTypes = map[string]string{"ps": "mysql", "mysqlce": "mysqlcerepl", "mariadb": "mariadbrepl"}

// mysqlTuningNodeFrame is the part of a standalone node's synthetic frame mysqlRedoOption reads.
func mysqlTuningNodeFrame(n designNode) designFrame {
	return designFrame{Type: mysqlTuningNodeTypes[n.Type], PSMajor: n.PSMajor, PSVersion: n.PSVersion,
		MySQLCEMajor: n.MySQLCEMajor, MySQLCEVersion: n.MySQLCEVersion, mysqlTuning: n.mysqlTuning}
}

// mysqlTuningIssues refuses a value the server would not start with, and warns about a buffer
// pool as big as the memory a server is given.
func mysqlTuningIssues(doc designDoc) []issue {
	var out []issue
	for _, f := range doc.Frames {
		if !mysqlTuningFrameTypes[f.Type] {
			continue
		}
		var mem []designNode
		for _, n := range doc.Nodes {
			if n.FrameID == f.ID && n.Role != "arbitrator" {
				mem = append(mem, n)
			}
		}
		out = append(out, mysqlTuningCheck("Cluster "+f.Label, f, mem)...)
	}
	for _, n := range doc.Nodes {
		if mysqlTuningNodeTypes[n.Type] != "" && n.FrameID == "" {
			out = append(out, mysqlTuningCheck("Node "+n.Label, mysqlTuningNodeFrame(n), []designNode{n})...)
		}
	}
	return out
}

// mysqlTuningCheck is mysqlTuningIssues for one cluster or server; servers are what its buffer
// pool is weighed against.
func mysqlTuningCheck(what string, f designFrame, servers []designNode) []issue {
	var out []issue
	bad := func(opt, v, want string) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: %s=%q is not %s", what, opt, v, want)})
	}
	size := "a size such as 512M or 2G"
	pool := int64(0)
	if v := f.InnoDBBufferPoolSize.str(); v != "" {
		if pool = mysqlSizeBytes(v); pool <= 0 {
			bad("innodb_buffer_pool_size", v, size)
		}
	}
	if v := f.InnoDBRedoLogCapacity.str(); v != "" && mysqlSizeBytes(v) <= 0 {
		bad(mysqlRedoOption(f), v, size)
	}
	if v := f.SyncBinlog.str(); v != "" {
		if _, err := strconv.ParseUint(v, 10, 32); err != nil {
			bad("sync_binlog", v, "a whole number from 0 to 4294967295")
		}
	}
	if v := f.InnoDBFlushLogAtTrxCommit.str(); v != "" && v != "0" && v != "1" && v != "2" {
		bad("innodb_flush_log_at_trx_commit", v, "0, 1 or 2")
	}
	if pool <= 0 {
		return out
	}
	for _, n := range servers {
		if n.MemoryGB > 0 && pool >= int64(n.MemoryGB)<<30 {
			out = append(out, issue{Level: "warning", Message: fmt.Sprintf(
				"%s: innodb_buffer_pool_size=%s is not smaller than %s's %d GiB memory limit; mysqld will be killed for running out of memory",
				what, f.InnoDBBufferPoolSize.str(), n.Label, n.MemoryGB)})
			break
		}
	}
	return out
}
