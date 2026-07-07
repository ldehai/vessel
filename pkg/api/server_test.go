package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andyliu/vessel/pkg/sandbox"
)

func newTestServer(t *testing.T) (*httptest.Server, *sandbox.Manager) {
	t.Helper()
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(sandbox.NewFakeDriver())
	ts := httptest.NewServer(NewServer(mgr))
	t.Cleanup(ts.Close)
	return ts, mgr
}

func TestHealthz(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp.StatusCode, err)
	}
}

func TestCreateAndExec(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/v1/sandboxes", "application/json",
		strings.NewReader(`{"driver":"fake","spec":{"name":"t"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ ID, State string }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.State != "running" {
		t.Fatalf("create: %+v", created)
	}

	resp, err = http.Post(ts.URL+"/v1/sandboxes/"+created.ID+"/exec",
		"application/json", strings.NewReader(`{"cmd":["echo","hi"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || out.Stdout != "fake-out" {
		t.Fatalf("exec: %+v", out)
	}
}

func TestExecNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := http.Post(ts.URL+"/v1/sandboxes/deadbeef/exec",
		"application/json", strings.NewReader(`{"cmd":["x"]}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateDefaultsDriver(t *testing.T) {
	ts, _ := newTestServer(t)
	// no driver field -> defaults to "process", which is not registered here
	resp, _ := http.Post(ts.URL+"/v1/sandboxes", "application/json",
		strings.NewReader(`{"spec":{}}`))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (unknown driver)", resp.StatusCode)
	}
}
