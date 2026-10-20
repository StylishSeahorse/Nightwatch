package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateOnce(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho v2\n")
	sum := sha256.Sum256(newBin)
	published := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := "/download/wasabi-agent-linux-" + distArch()
		switch {
		case !published:
			http.NotFound(w, r)
		case r.URL.Path == name+".sha256":
			w.Write([]byte(hex.EncodeToString(sum[:]) + "  wasabi-agent-linux\n"))
		case r.URL.Path == name:
			w.Write(newBin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "wasabi-agent")
	os.WriteFile(exe, []byte("#!/bin/sh\necho v1\n"), 0o755)
	a := &Agent{api: &apiClient{base: srv.URL, http: srv.Client()}, running: map[string]*activeRun{}}
	ctx := context.Background()

	// A backup is running: never swap the binary under it.
	a.running["r"] = &activeRun{runID: "r"}
	if ok, err := a.updateOnce(ctx, exe); ok || err != nil {
		t.Fatalf("while running: installed=%v err=%v", ok, err)
	}
	delete(a.running, "r")

	if ok, err := a.updateOnce(ctx, exe); !ok || err != nil {
		t.Fatalf("idle: installed=%v err=%v", ok, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatalf("binary not replaced: %q", b)
	}
	// Already current: nothing to do.
	if ok, err := a.updateOnce(ctx, exe); ok || err != nil {
		t.Fatalf("current: installed=%v err=%v", ok, err)
	}
	// A dashboard without agent builds is not an error.
	published = false
	if ok, err := a.updateOnce(ctx, exe); ok || err != nil {
		t.Fatalf("unpublished: installed=%v err=%v", ok, err)
	}
}

func TestUpdateRejectsBadChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Ext(r.URL.Path) == ".sha256" {
			w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  x\n"))
			return
		}
		w.Write([]byte("#!/bin/sh\necho evil\n"))
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "wasabi-agent")
	os.WriteFile(exe, []byte("old"), 0o755)
	a := &Agent{api: &apiClient{base: srv.URL, http: srv.Client()}, running: map[string]*activeRun{}}
	if ok, err := a.updateOnce(context.Background(), exe); ok || err == nil {
		t.Fatalf("installed=%v err=%v; want a checksum error", ok, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("current agent must be left untouched")
	}
}
