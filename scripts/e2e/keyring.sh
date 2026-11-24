# shellcheck shell=bash
# keyring.sh — e2e matrix: keyring-file encryption, the encryption options, XtraBackup backups
# and restores of encrypted servers, and the keyring endpoints. Sourced by scripts/e2e.sh, whose
# functions it uses. Two stacks, one after the other, each removed when its checks are done.

# rows prints the row count of the probe table.
rows() { e2e_sql "$1" "$2" "SELECT COUNT(*) FROM t.e" | tail -1; }
probe() {
  e2e_sql "$1" "$2" "CREATE DATABASE IF NOT EXISTS t; CREATE TABLE IF NOT EXISTS t.e (id INT PRIMARY KEY AUTO_INCREMENT, v VARCHAR(20)); INSERT INTO t.e (v) VALUES ('one'), ('two')" >/dev/null
}
encrypted() { e2e_sql "$1" "$2" "SELECT CREATE_OPTIONS FROM information_schema.tables WHERE table_schema='t' AND table_name='e'" | tail -1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)" 2>/dev/null; }
first_backup() { e2e_api GET "$1/xb/backups" | json 'd["backups"][0]["name"]'; }
newest_backup() { e2e_api GET "$1/xb/backups" | json 'd["backups"][-1]["name"]'; }

E='opt.keyringFile=true,opt.encryptBinlog=true,opt.encryptRedoUndo=true'

# --- what validation refuses -------------------------------------------------------------------
e2e_refused kr-refuse-pitr "exclude each other" --node seaweedfs --node "ps,version=8.4,backup,$E,name=ps84"
e2e_refused kr-refuse-temp "is a Percona Server option" --node "mysql,version=9.7,$E,opt.encryptTemp=true,name=ce97"

# --- standalone servers --------------------------------------------------------------------------
if e2e_up kr-one --node seaweedfs \
  --node "ps,version=8.4,backup,opt.xbPitr=false,$E,opt.encryptTemp=true,name=ps84" \
  --node "ps,version=8.0,backup,opt.keyringFile=true,opt.encryptRedoUndo=true,name=ps80" \
  --node "mysql,version=9.7,$E,name=ce97"; then
  S=kr-one
  ID=$(e2e_stack_id $S)

  for n in ps84 ps80 ce97; do
    probe $S $n
    e2e_expect "$n: a new table is encrypted" "$(encrypted $S $n)" "ENCRYPTION='Y'"
  done
  e2e_expect "ps84: every option is on" "$(e2e_sql $S ps84 "SELECT CONCAT_WS(',', @@binlog_encryption, @@innodb_redo_log_encrypt, @@innodb_undo_log_encrypt, @@innodb_temp_tablespace_encrypt)" | tail -1)" "1,1,1,1"
  e2e_expect "ce97: binary log and redo/undo encryption are on" "$(e2e_sql $S ce97 "SELECT CONCAT_WS(',', @@binlog_encryption, @@innodb_redo_log_encrypt, @@innodb_undo_log_encrypt)" | tail -1)" "1,1,1"

  # Full + incremental of a component keyring (8.4), restored to the incremental.
  N=/api/stacks/$ID/nodes/n-ps84
  e2e_contains "ps84: full backup" "$(e2e_api POST $N/xb/backup '{"type":"full"}')" '"keyring":"file"'
  e2e_sql $S ps84 "INSERT INTO t.e (v) VALUES ('three')" >/dev/null
  e2e_contains "ps84: incremental backup" "$(e2e_api POST $N/xb/backup '{"type":"incremental"}')" '"type":"incremental"'
  INCR=$(newest_backup $N)
  e2e_sql $S ps84 "INSERT INTO t.e (v) VALUES ('four'); DROP TABLE t.e" >/dev/null
  BODY=$(printf '{"mode":"backup","name":"%s"}' "$INCR")
  e2e_contains "ps84: restore to the incremental" "$(e2e_api POST $N/xb/restore "$BODY")" '"status":"ok"'
  e2e_expect "ps84: rows after the restore" "$(rows $S ps84)" 3
  e2e_exec $S ps84 "systemctl restart mysqld" >/dev/null
  e2e_expect "ps84: rows after a restart" "$(rows $S ps84)" 3

  # Point-in-time restore of a plugin keyring (8.0).
  N=/api/stacks/$ID/nodes/n-ps80
  e2e_contains "ps80: full backup" "$(e2e_api POST $N/xb/backup '{"type":"full"}')" '"keyring":"file"'
  e2e_sql $S ps80 "INSERT INTO t.e (v) VALUES ('three')" >/dev/null
  e2e_contains "ps80: restore to the latest point" "$(e2e_api POST $N/xb/restore '{"mode":"latest"}')" '"status":"ok"'
  e2e_expect "ps80: rows after the restore" "$(rows $S ps80)" 3

  # The keyring endpoints.
  N=/api/stacks/$ID/nodes/n-ps84
  e2e_contains "ps84: keyring status" "$(e2e_api GET $N/mysql/keyring)" '"provider":"file"'
  e2e_contains "ps84: rotate rotates the binary log key too" "$(e2e_api POST $N/mysql/keyring/rotate)" "binary log master key"
  e2e_expect "ps84: the rotation is on record" "$(e2e_api GET $N/mysql/keyring | json 'bool(d["rotatedAt"])')" True
  SIZE=$(e2e_api GET $N/mysql/keyring/file | wc -c | tr -d ' ')
  e2e_expect "ps84: the keyring file downloads" "$([ "$SIZE" -gt 0 ] && echo yes)" yes
  e2e_expect "ps84: rows after the rotation" "$(rows $S ps84)" 3

  [ "${E2E_KEEP:-0}" = 1 ] || "$BIN" rm $S >/dev/null 2>&1
fi

# --- PXC -----------------------------------------------------------------------------------------
if e2e_up kr-pxc --node seaweedfs \
  --node "pxc:3,version=8.4,backup,opt.keyringFile=true,opt.encryptRedoUndo=true,opt.encryptTemp=true"; then
  S=kr-pxc
  ID=$(e2e_stack_id $S)
  F=/api/stacks/$ID/frames/c-pxc-cluster-01
  probe $S pxc-1
  e2e_contains "pxc: full backup" "$(e2e_api POST $F/xb/backup '{"type":"full"}')" '"keyring":"file"'
  e2e_sql $S pxc-2 "INSERT INTO t.e (v) VALUES ('three')" >/dev/null
  e2e_contains "pxc: incremental backup" "$(e2e_api POST $F/xb/backup '{"type":"incremental"}')" '"type":"incremental"'
  e2e_sql $S pxc-1 "INSERT INTO t.e (v) VALUES ('four')" >/dev/null
  sleep 2
  e2e_contains "pxc: restore to the latest point" "$(e2e_api POST $F/xb/restore '{"mode":"latest"}')" '"status":"ok"'
  for n in pxc-1 pxc-2 pxc-3; do
    e2e_expect "$n: rows after the restore" "$(rows $S $n)" 4
    e2e_expect "$n: still encrypted" "$(encrypted $S $n)" "ENCRYPTION='Y'"
    e2e_expect "$n: in a cluster of 3" "$(e2e_sql $S $n "SHOW STATUS LIKE 'wsrep_cluster_size'" | awk '{print $2}' | tail -1)" 3
  done
  e2e_contains "pxc-2: rotates its own master key" "$(e2e_api POST /api/stacks/$ID/nodes/n-pxc-cluster-01-2/mysql/keyring/rotate)" "InnoDB master key"
  [ "${E2E_KEEP:-0}" = 1 ] || "$BIN" rm $S >/dev/null 2>&1
fi

# --- OpenBao keyrings: the vault component (8.4) and the vault plugin (8.0) ----------------------
if e2e_up kr-bao --node seaweedfs --node openbao \
  --node "ps,version=8.4,backup,vault,opt.xbPitr=false,opt.encryptRedoUndo=true,name=bao84" \
  --node "ps,version=8.0,backup,vault,name=bao80"; then
  S=kr-bao
  ID=$(e2e_stack_id $S)
  for n in bao84 bao80; do
    N=/api/stacks/$ID/nodes/n-$n
    probe $S $n
    e2e_expect "$n: a new table is encrypted" "$(encrypted $S $n)" "ENCRYPTION='Y'"
    e2e_contains "$n: full backup" "$(e2e_api POST $N/xb/backup '{"type":"full"}')" '"keyring":"vault"'
    FULL=$(first_backup $N)
    e2e_sql $S $n "INSERT INTO t.e (v) VALUES ('three')" >/dev/null
    BODY=$(printf '{"mode":"backup","name":"%s"}' "$FULL")
    e2e_contains "$n: restore the full backup" "$(e2e_api POST $N/xb/restore "$BODY")" '"status":"ok"'
    e2e_expect "$n: rows after the restore" "$(rows $S $n)" 2
    e2e_exec $S $n "systemctl restart mysqld" >/dev/null
    e2e_expect "$n: rows after a restart" "$(rows $S $n)" 2
    e2e_contains "$n: rotates its master key in OpenBao" "$(e2e_api POST $N/mysql/keyring/rotate)" "InnoDB master key"
    e2e_contains "$n: has no keyring file to download" "$(e2e_api GET $N/mysql/keyring/file)" "not in a keyring file"
  done
  [ "${E2E_KEEP:-0}" = 1 ] || "$BIN" rm $S >/dev/null 2>&1
fi
