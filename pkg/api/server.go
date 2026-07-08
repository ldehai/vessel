// Package api exposes the agent-facing REST API. gRPC + SDK land in M3.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/ldehai/vessel/pkg/sandbox"
)

type Server struct {
	mgr *sandbox.Manager
	mux *http.ServeMux
}

func NewServer(mgr *sandbox.Manager) *Server {
	s := &Server{mgr: mgr, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /v1/sandboxes", s.list)
	s.mux.HandleFunc("POST /v1/sandboxes", s.create)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.exec)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", s.snapshot)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/fork", s.fork)
	s.mux.HandleFunc("POST /v1/sandboxes/restore", s.restore)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

type createReq struct {
	Driver string        `json:"driver"`
	Spec   *sandbox.Spec `json:"spec"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Driver == "" {
		req.Driver = "process"
	}
	inst, err := s.mgr.Create(r.Context(), req.Driver, req.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"id": inst.ID(), "state": string(inst.State())})
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	items := []item{}
	for _, inst := range s.mgr.List() {
		items = append(items, item{inst.ID(), string(inst.State())})
	}
	writeJSON(w, items)
}

type execReq struct {
	Cmd []string `json:"cmd"`
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cmd) == 0 {
		http.Error(w, "bad exec request", http.StatusBadRequest)
		return
	}
	var stdout, stderr bytes.Buffer
	code, err := inst.Exec(r.Context(), req.Cmd, &stdout, &stderr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"exit_code": code,
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
	})
}

type snapshotReq struct {
	Path string `json:"path"`
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		http.Error(w, "bad snapshot request: path required", http.StatusBadRequest)
		return
	}
	if err := s.mgr.Snapshot(r.Context(), r.PathValue("id"), req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"snapshot": req.Path})
}

func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	var req snapshotReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		http.Error(w, "bad fork request: path required", http.StatusBadRequest)
		return
	}
	clone, err := s.mgr.Fork(r.Context(), r.PathValue("id"), req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"id": clone.ID(), "state": string(clone.State())})
}

type restoreReq struct {
	Driver string `json:"driver"`
	Path   string `json:"path"`
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req restoreReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		http.Error(w, "bad restore request: path required", http.StatusBadRequest)
		return
	}
	if req.Driver == "" {
		req.Driver = "cloudhypervisor"
	}
	inst, err := s.mgr.RestoreFrom(r.Context(), req.Driver, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"id": inst.ID(), "state": string(inst.State())})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
