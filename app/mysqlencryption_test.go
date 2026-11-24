package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMySQLEncryptionOptions(t *testing.T) {
	if o := mysqlEncryptionOptions(mysqlEncryption{}); o != "" {
		t.Errorf("nothing chosen, got %q", o)
	}
	o := mysqlEncryptionOptions(mysqlEncryption{EncryptBinlog: true, EncryptRedoUndo: true, EncryptTemp: true})
	for _, want := range []string{"binlog_encryption=ON", "innodb_redo_log_encrypt=ON", "innodb_undo_log_encrypt=ON", "innodb_temp_tablespace_encrypt=ON"} {
		if !strings.Contains(o, want+"\n") {
			t.Errorf("missing %s:\n%s", want, o)
		}
	}
}

func TestMySQLEncryptionIssues(t *testing.T) {
	all := mysqlEncryption{EncryptBinlog: true, EncryptRedoUndo: true, EncryptTemp: true}
	if got := mysqlEncryptionIssues("x", mysqlEncryption{}, false, false, "8.0", "", false); len(got) != 0 {
		t.Errorf("nothing chosen: %v", got)
	}
	if got := mysqlEncryptionIssues("x", all, false, false, "8.4", "", false); len(got) != 1 || !strings.Contains(got[0].Message, "without a keyring") {
		t.Errorf("no keyring: %v", got)
	}
	if got := mysqlEncryptionIssues("x", all, true, false, "8.4", "", false); len(got) != 0 {
		t.Errorf("8.4 has them all: %v", got)
	}
	if got := mysqlEncryptionIssues("x", all, true, false, "5.7", "", false); len(got) != 4 {
		t.Errorf("5.7 has none of the four features: %v", got)
	}
	if got := mysqlEncryptionIssues("x", mysqlEncryption{EncryptTemp: true}, true, true, "8.4", "", false); len(got) != 1 || !strings.Contains(got[0].Message, "Percona Server option") {
		t.Errorf("MySQL Community has no innodb_temp_tablespace_encrypt: %v", got)
	}
	if got := mysqlEncryptionIssues("x", mysqlEncryption{EncryptBinlog: true, EncryptRedoUndo: true}, true, true, "9.7", "", false); len(got) != 0 {
		t.Errorf("MySQL Community 9.7 has binary log and redo/undo encryption: %v", got)
	}
	if got := mysqlEncryptionIssues("x", mysqlEncryption{EncryptTemp: true}, true, false, "8.0", "8.0.15-6.1", false); len(got) != 1 {
		t.Errorf("8.0.15 has no innodb_temp_tablespace_encrypt: %v", got)
	}
	if got := mysqlEncryptionIssues("x", mysqlEncryption{EncryptBinlog: true}, true, false, "8.4", "", true); len(got) != 1 || !strings.Contains(got[0].Message, "mysqlbinlog") {
		t.Errorf("binary log encryption with PITR: %v", got)
	}
	if got := mysqlEncryptionIssues("x", mysqlEncryption{EncryptRedoUndo: true}, true, false, "8.4", "", true); len(got) != 0 {
		t.Errorf("redo/undo with PITR is fine: %v", got)
	}
}

// The options sit on nodes and frames under their own JSON names, and the synthetic frame of a
// standalone server carries them to the shared keyring path.
func TestMySQLEncryptionJSON(t *testing.T) {
	var n designNode
	if err := json.Unmarshal([]byte(`{"type":"ps","keyringFile":true,"encryptBinlog":true,"encryptTemp":true}`), &n); err != nil {
		t.Fatal(err)
	}
	if !n.EncryptBinlog || !n.EncryptTemp || n.EncryptRedoUndo {
		t.Errorf("node: %+v", n.mysqlEncryption)
	}
	var f designFrame
	json.Unmarshal([]byte(`{"type":"pxc","encryptRedoUndo":true}`), &f)
	if !f.EncryptRedoUndo {
		t.Error("frame did not read encryptRedoUndo")
	}
	if got := strings.Join(f.mysqlEncryption.variables(), " "); got != "innodb_redo_log_encrypt innodb_undo_log_encrypt" {
		t.Errorf("variables: %s", got)
	}
}

func TestMySQLEncryptionVerifyScript(t *testing.T) {
	for _, want := range []string{"SELECT @@GLOBAL.$v", "MASTER_KEY", "exit 1"} {
		if !strings.Contains(mysqlEncryptionVerifyScript, want) {
			t.Errorf("verify script is missing %q", want)
		}
	}
}
