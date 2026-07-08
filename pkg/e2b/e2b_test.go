package e2b

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ldehai/vessel/pkg/sandbox"
)

func newTestHandler(t *testing.T) (*httptest.Server, *Handler, *sandbox.FakeDriver) {
	t.Helper()
	d := sandbox.NewFakeDriver()
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(d)
	h := NewHandler(mgr, "fake")
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, h, d
}

func TestCreateFreshSandbox(t *testing.T) {
	ts, _, _ := newTestHandler(t)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json",
		strings.NewReader(`{"templateID":"base","metadata":{"user":"a"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var sb sandboxResp
	_ = json.NewDecoder(resp.Body).Decode(&sb)
	if sb.SandboxID == "" || sb.TemplateID != "base" || sb.EnvdVersion == "" {
		t.Fatalf("resp = %+v", sb)
	}
	if sb.EnvdAccessToken != nil {
		t.Fatal("non-secure sandbox must have null envdAccessToken")
	}
}

func TestCreateMissingTemplateID(t *testing.T) {
	ts, _, _ := newTestHandler(t)
	resp, _ := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var e e2bError
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Code != 400 || e.Message == "" {
		t.Fatalf("error body = %+v", e)
	}
}

func TestCreateFromRegisteredTemplateUsesRestore(t *testing.T) {
	ts, h, d := newTestHandler(t)
	// FakeDriver.Restore only succeeds for a path present in its Snapshots.
	d.Snapshots["/snap/py"] = []byte("template")
	h.RegisterTemplate("python-3.12", Template{Driver: "fake", SnapshotPath: "/snap/py"})

	resp, err := http.Post(ts.URL+"/sandboxes", "application/json",
		strings.NewReader(`{"templateID":"python-3.12"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (restore path)", resp.StatusCode)
	}
	var sb sandboxResp
	_ = json.NewDecoder(resp.Body).Decode(&sb)
	if sb.TemplateID != "python-3.12" {
		t.Fatalf("resp = %+v", sb)
	}
}

func TestListAndKill(t *testing.T) {
	ts, _, _ := newTestHandler(t)
	resp, _ := http.Post(ts.URL+"/sandboxes", "application/json",
		strings.NewReader(`{"templateID":"base"}`))
	var sb sandboxResp
	_ = json.NewDecoder(resp.Body).Decode(&sb)

	// list contains it
	lr, _ := http.Get(ts.URL + "/sandboxes")
	var list []sandboxResp
	_ = json.NewDecoder(lr.Body).Decode(&list)
	if len(list) != 1 || list[0].SandboxID != sb.SandboxID {
		t.Fatalf("list = %+v", list)
	}

	// kill it
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, nil)
	kr, err := http.DefaultClient.Do(req)
	if err != nil || kr.StatusCode != http.StatusNoContent {
		t.Fatalf("kill: %d %v", kr.StatusCode, err)
	}

	// killing unknown -> 404
	req2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/nope", nil)
	kr2, _ := http.DefaultClient.Do(req2)
	if kr2.StatusCode != http.StatusNotFound {
		t.Fatalf("kill unknown = %d, want 404", kr2.StatusCode)
	}
}
