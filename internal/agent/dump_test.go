package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stylishseahorse/rclone-backup-manager/internal/proto"
)

// fakeBin writes an executable shell script and returns its path.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func dumpHarness(t *testing.T, docker, rclone string) (*Agent, *runLogger, *fakeDashboard) {
	f := &fakeDashboard{}
	lg := newTestLogger(t, f)
	a := &Agent{cfg: &Config{DockerPath: docker, RclonePath: rclone}, conf: &proto.AgentConfig{Name: "box"}}
	return a, lg, f
}

func runDump(t *testing.T, a *Agent, lg *runLogger, limit time.Duration) (error, time.Duration) {
	t.Helper()
	job := &proto.JobConfig{ID: "j", Wasabi: wasabi}
	d := proto.DumpConfig{Name: "db", Container: "mysql", User: "root", UseContainerEnv: true}
	t0 := time.Now()
	done := make(chan error, 1)
	go func() { done <- a.dumpOne(context.Background(), job, d, "2026-10-17T000000Z", lg) }()
	select {
	case err := <-done:
		return err, time.Since(t0)
	case <-time.After(limit):
		t.Fatalf("dump did not finish within %s: the pipeline is wedged", limit)
		return nil, 0
	}
}

// The bug that wedged an agent for hours: the upload fails while mysqldump is
// still producing output. The dump must end promptly, not block forever.
func TestDumpEndsWhenUploadDiesFirst(t *testing.T) {
	docker := fakeBin(t, "docker", `yes "INSERT INTO t VALUES (1,'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx');"`) // endless SQL
	rclone := fakeBin(t, "rclone", `case "$1" in rcat) echo "ERROR : multipart upload failed to initialise: AccessDenied" >&2; exit 1;; *) exit 0;; esac`)
	a, lg, _ := dumpHarness(t, docker, rclone)
	err, took := runDump(t, a, lg, 20*time.Second)
	if err == nil || !strings.Contains(err.Error(), "upload to Wasabi failed") {
		t.Fatalf("err = %v", err)
	}
	t.Logf("ended in %s: %v", took.Round(time.Millisecond), err)
}

// A database that hangs without output is stopped by the stall watchdog.
func TestDumpStallWatchdog(t *testing.T) {
	old := dumpStallTimeout
	dumpStallTimeout = 2 * time.Second
	defer func() { dumpStallTimeout = old }()
	docker := fakeBin(t, "docker", `sleep 120`)
	rclone := fakeBin(t, "rclone", `case "$1" in rcat) cat >/dev/null;; *) exit 0;; esac`)
	a, lg, _ := dumpHarness(t, docker, rclone)
	err, took := runDump(t, a, lg, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("err = %v", err)
	}
	if took > 10*time.Second {
		t.Errorf("watchdog took %s", took)
	}
}

// Cancelling the run stops a dump that is streaming happily.
func TestDumpCancel(t *testing.T) {
	docker := fakeBin(t, "docker", `yes "INSERT INTO t VALUES (1);"`)
	rclone := fakeBin(t, "rclone", `case "$1" in rcat) cat >/dev/null;; *) exit 0;; esac`)
	a, lg, _ := dumpHarness(t, docker, rclone)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.dumpOne(ctx, &proto.JobConfig{ID: "j", Wasabi: wasabi}, proto.DumpConfig{Name: "db", Container: "c", User: "root", UseContainerEnv: true}, "2026-10-17T000000Z", lg)
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled dump reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancel did not stop the dump")
	}
}

// A failure that repeats one line thousands of times is collapsed, and no run
// can log more than maxRunLines.
func TestRunLoggerCollapsesRepeatsAndCaps(t *testing.T) {
	f := &fakeDashboard{}
	lg := newTestLogger(t, f)
	lg.start()
	for i := 0; i < 5000; i++ {
		lg.add("stderr", "ERROR : retrying")
	}
	lg.add("stdout", "done")
	lg.close(context.Background())
	if len(f.lines) != 3 || !strings.Contains(f.lines[1].Line, "repeated 4999 more times") {
		t.Fatalf("lines = %+v", f.lines)
	}
	f2 := &fakeDashboard{}
	lg2 := newTestLogger(t, f2)
	lg2.start()
	for i := 0; i < maxRunLines+500; i++ {
		lg2.add("stdout", "line "+strings.Repeat("x", i%7)+string(rune('a'+i%26)))
	}
	lg2.close(context.Background())
	if n := len(f2.lines); n > maxRunLines+1 {
		t.Errorf("logged %d lines, cap is %d", n, maxRunLines)
	}
}
