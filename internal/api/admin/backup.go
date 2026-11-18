package admin

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

func (a *API) registerBackup(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/backup", a.protected(a.downloadBackup))
}

// downloadBackup streams a consistent online copy of the database (VACUUM INTO
// a temporary file next to the database, then removed).
func (a *API) downloadBackup(w http.ResponseWriter, r *http.Request, act *actor) {
	dir := a.BackupDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, ".llmgw-backup-*.db")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot create temporary file: "+err.Error())
		return
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // VACUUM INTO requires a non-existing file
	defer os.Remove(name)
	if err := a.Store.Backup(r.Context(), name); err != nil {
		a.Log.Error("backup", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "backup failed: "+err.Error())
		return
	}
	src, err := os.Open(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer src.Close()
	a.audit(r, act.Name, "backup.download", "database", "", nil)
	fname := "llmgw-" + a.Clock.Now().UTC().Format("20060102-150405") + ".db"
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(fname)+`"`)
	if st, err := src.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, src)
}
