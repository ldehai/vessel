package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ldehai/vessel/pkg/sandbox"
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

func TestDeleteSandbox(t *testing.T) {
	ts, mgr := newTestServer(t)

	resp, err := http.Post(ts.URL+"/v1/sandboxes", "application/json",
		strings.NewReader(`{"driver":"fake","spec":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	if _, ok := mgr.Get(created.ID); !ok {
		t.Fatal("sandbox not created")
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sandboxes/"+created.ID, nil)
	dr, err := http.DefaultClient.Do(req)
	if err != nil || dr.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %v", dr.StatusCode, err)
	}
	if _, ok := mgr.Get(created.ID); ok {
		t.Fatal("sandbox still tracked after DELETE")
	}

	// Deleting an unknown id -> 404.
	req2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sandboxes/nope", nil)
	dr2, _ := http.DefaultClient.Do(req2)
	if dr2.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", dr2.StatusCode)
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

func TestSnapshotAndForkEndpoints(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, _ := http.Post(ts.URL+"/v1/sandboxes", "application/json",
		strings.NewReader(`{"driver":"fake","spec":{}}`))
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)

	resp, err := http.Post(ts.URL+"/v1/sandboxes/"+created.ID+"/snapshot",
		"application/json", strings.NewReader(`{"path":"/snap/x"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("snapshot: %d %v", resp.StatusCode, err)
	}

	resp, err = http.Post(ts.URL+"/v1/sandboxes/"+created.ID+"/fork",
		"application/json", strings.NewReader(`{"path":"/snap/y"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("fork: %d %v", resp.StatusCode, err)
	}
	var clone struct{ ID, State string }
	_ = json.NewDecoder(resp.Body).Decode(&clone)
	if clone.ID == "" || clone.ID == created.ID || clone.State != "running" {
		t.Fatalf("clone: %+v", clone)
	}

	// missing path -> 400
	resp, _ = http.Post(ts.URL+"/v1/sandboxes/"+created.ID+"/fork",
		"application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fork without path: %d, want 400", resp.StatusCode)
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
