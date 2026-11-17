package main

import (
	"fmt"
	"net/http"
)

// clihelp.go — the dbcanvas command's usage, served to the API page. The page shows what this
// very build would print for `dbcanvas help`, from the same text (cliUsage), so the list cannot
// drift from the commands that exist.

func (a *App) handleCLIHelp(w http.ResponseWriter, r *http.Request) {
	dir, _ := dataDir()
	writeJSON(w, http.StatusOK, map[string]string{"help": fmt.Sprintf(cliUsage, dir)})
}
