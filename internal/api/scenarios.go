package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/maglo/qemu-lab-manager/labview/internal/scenario"
)

func (s *Server) handleScenarioRuns(w http.ResponseWriter, r *http.Request) {
	if s.scenarios == nil {
		writeError(w, http.StatusNotImplemented, "scenario runner is not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.scenarios.List()})
}

func (s *Server) handleScenarioRunCreate(w http.ResponseWriter, r *http.Request) {
	if s.scenarios == nil {
		writeError(w, http.StatusNotImplemented, "scenario runner is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body: %s", err.Error())
		return
	}
	doc, err := scenario.ParseDocument(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	run, err := s.scenarios.Start(r.Context(), doc)
	if err != nil {
		writeError(w, http.StatusConflict, "%s", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleScenarioRun(w http.ResponseWriter, r *http.Request) {
	if s.scenarios == nil {
		writeError(w, http.StatusNotImplemented, "scenario runner is not configured")
		return
	}
	id := r.PathValue("id")
	run, ok := s.scenarios.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "run %q is not known", id)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleScenarioRunCancel(w http.ResponseWriter, r *http.Request) {
	if s.scenarios == nil {
		writeError(w, http.StatusNotImplemented, "scenario runner is not configured")
		return
	}
	id := r.PathValue("id")
	run, err := s.scenarios.Cancel(id)
	if err != nil {
		if scenario.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "run %q is not known", id)
			return
		}
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleScenarioEvidence(w http.ResponseWriter, r *http.Request) {
	if s.scenarios == nil {
		writeError(w, http.StatusNotImplemented, "scenario runner is not configured")
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	path, err := s.scenarios.EvidenceFile(id, name)
	if err != nil {
		if scenario.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "evidence %q for run %q is not known", name, id)
			return
		}
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "evidence %q for run %q is not known", name, id)
		return
	}
	defer f.Close()
	switch filepath.Ext(name) {
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	case ".cast":
		w.Header().Set("Content-Type", "application/x-asciicast")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(name)+`"`)
	_, _ = io.Copy(w, f)
}
