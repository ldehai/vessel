// Package e2b provides an E2B-compatible control-plane REST API, so tools
// and SDKs built for E2B can target a self-hosted vessel by changing only
// the API base URL.
//
// Scope: this implements the E2B *platform* control plane — sandbox
// lifecycle (POST/GET/DELETE /sandboxes), matching E2B's request/response
// schemas and status codes (see e2b.dev OpenAPI). E2B's data plane
// (in-sandbox `envd` filesystem/process over gRPC) is a separate, larger
// surface tracked as follow-up; vessel's native REST (/v1/sandboxes/.../exec)
// covers execution today and is mounted alongside these routes.
//
// templateID mapping: a templateID registered with RegisterTemplate resolves
// to a vessel driver + snapshot path and is served via the fast restore
// path (sub-100ms). The reserved id "base" (and the empty templateID) create
// a fresh sandbox instead.
package e2b

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ldehai/vessel/pkg/sandbox"
)

const envdVersion = "vessel-0.2"

// Template maps an E2B templateID to a vessel restore source.
type Template struct {
	Driver       string
	SnapshotPath string
}

// Handler serves the E2B-compatible routes against a sandbox.Manager.
type Handler struct {
	mgr           *sandbox.Manager
	mux           *http.ServeMux
	defaultDriver string

	mu        sync.RWMutex
	templates map[string]Template
	meta      map[string]sandboxMeta // sandboxID -> bookkeeping for GET responses
}

type sandboxMeta struct {
	templateID string
	createdAt  time.Time
}

func NewHandler(mgr *sandbox.Manager, defaultDriver string) *Handler {
	h := &Handler{
		mgr:           mgr,
		mux:           http.NewServeMux(),
		defaultDriver: defaultDriver,
		templates:     map[string]Template{},
		meta:          map[string]sandboxMeta{},
	}
	h.mux.HandleFunc("POST /sandboxes", h.create)
	h.mux.HandleFunc("GET /sandboxes", h.list)
	h.mux.HandleFunc("DELETE /sandboxes/{id}", h.kill)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// RegisterTemplate binds an E2B templateID to a vessel snapshot.
func (h *Handler) RegisterTemplate(id string, t Template) {
	h.mu.Lock()
	h.templates[id] = t
	h.mu.Unlock()
}

// --- E2B schemas (subset) ---

type newSandbox struct {
	TemplateID string            `json:"templateID"`
	Timeout    int32             `json:"timeout,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	EnvVars    map[string]string `json:"envVars,omitempty"`
}

type sandboxResp struct {
	TemplateID      string  `json:"templateID"`
	SandboxID       string  `json:"sandboxID"`
	ClientID        string  `json:"clientID"`
	EnvdVersion     string  `json:"envdVersion"`
	EnvdAccessToken *string `json:"envdAccessToken"`
}

type e2bError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req newSandbox
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TemplateID == "" {
		writeErr(w, http.StatusBadRequest, "templateID is required")
		return
	}

	inst, err := h.resolveAndCreate(r, req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.mu.Lock()
	h.meta[inst.ID()] = sandboxMeta{templateID: req.TemplateID, createdAt: time.Now()}
	h.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sandboxResp{
		TemplateID:      req.TemplateID,
		SandboxID:       inst.ID(),
		ClientID:        "vessel",
		EnvdVersion:     envdVersion,
		EnvdAccessToken: nil, // non-secure: envd endpoints need no auth
	})
}

func (h *Handler) resolveAndCreate(r *http.Request, req newSandbox) (sandbox.Instance, error) {
	h.mu.RLock()
	tmpl, registered := h.templates[req.TemplateID]
	h.mu.RUnlock()

	// Registered template -> fast restore path.
	if registered && tmpl.SnapshotPath != "" {
		driver := tmpl.Driver
		if driver == "" {
			driver = h.defaultDriver
		}
		return h.mgr.RestoreFrom(r.Context(), driver, tmpl.SnapshotPath)
	}

	// "base" or unknown template -> fresh sandbox.
	spec := &sandbox.Spec{Name: req.TemplateID, Env: req.EnvVars}
	if req.Timeout > 0 {
		spec.Timeout = time.Duration(req.Timeout) * time.Second
	}
	return h.mgr.Create(r.Context(), h.defaultDriver, spec)
}

func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := []sandboxResp{}
	for _, inst := range h.mgr.List() {
		m := h.meta[inst.ID()]
		out = append(out, sandboxResp{
			TemplateID:  m.templateID,
			SandboxID:   inst.ID(),
			ClientID:    "vessel",
			EnvdVersion: envdVersion,
		})
	}
	writeJSON(w, out)
}

func (h *Handler) kill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, ok := h.mgr.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if err := inst.Stop(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.mu.Lock()
	delete(h.meta, id)
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(e2bError{Code: code, Message: msg})
}
