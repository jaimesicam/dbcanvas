package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"dbcanvas/internal/seal"
)

// cmd_rotatekey.go — `dbcanvas rotate-key`: re-encrypt the database's sealed values (the node
// passwords, stack designs, generated secrets, session transcripts; encryption.go) under a new
// key. The database must be closed for it, so the service is stopped first, and the engine lock
// held, so that no command starts one meanwhile. The old key is kept beside the new one as
// <key>.old until it is deleted.

func cliRotateKey(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dbcanvas rotate-key")
	}
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if ctlReachable(dir) {
		fmt.Println("Stopping dbcanvas to rotate the key (stacks keep running)…")
		if err := (ctl{c: ctlClient(dir)}).do("POST", "/api/solo/stop", nil, nil); err != nil {
			return err
		}
	}
	var lock *engineLock
	for i := 0; i < 50; i++ {
		if lock, err = lockEngine(dir); !errors.Is(err, errEngineRunning) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("dbcanvas is still running and holds the database: %v", err)
	}
	defer lock.release()
	dbPath := filepath.Join(dir, "dbcanvas.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database at %s yet: nothing to re-encrypt", dbPath)
	}
	log.SetOutput(os.Stdout)
	log.SetFlags(0)
	if err := rotateKey(dbPath); err != nil {
		return err
	}
	fmt.Printf("Back up the new key, %s, then delete %s.old. The next dbcanvas command starts the service again.\n",
		seal.KeyPath(dbPath), seal.KeyPath(dbPath))
	return nil
}
