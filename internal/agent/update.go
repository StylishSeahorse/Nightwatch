package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Self-update: the dashboard serves the agent binary it was deployed with
// (/download/wasabi-agent-linux-<arch>, the same file the installer fetches).
// The agent compares that file's checksum with its own executable's and, when
// they differ and no backup is running, swaps the binary in place and exits;
// systemd (Restart=always) starts the new version. Redeploying the dashboard
// is therefore enough to update every installed agent.

var (
	updateFirstCheck = time.Minute
	updateEvery      = 15 * time.Minute
)

// distArch names this build in the dashboard's /download file names.
func distArch() string {
	if runtime.GOARCH == "arm" {
		return "armv7"
	}
	return runtime.GOARCH
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// updateLoop checks for a new agent build and, after installing one, calls
// restart so Run returns and the service manager starts the new binary.
func (a *Agent) updateLoop(ctx context.Context, restart func()) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Printf("auto-update disabled: cannot locate own executable: %v", err)
		return
	}
	wait := updateFirstCheck
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = updateEvery
		installed, err := a.updateOnce(ctx, exe)
		if err != nil {
			log.Printf("auto-update: %v", err)
			continue
		}
		if installed {
			log.Printf("auto-update: new agent installed, restarting")
			restart()
			return
		}
	}
}

// updateOnce installs the dashboard's agent binary over exe when it differs.
// It only replaces the binary while no run is in progress, and leaves exe
// untouched on any failure.
func (a *Agent) updateOnce(ctx context.Context, exe string) (bool, error) {
	name := "wasabi-agent-linux-" + distArch()
	var sumText string
	err := a.api.download(ctx, "/download/"+name+".sha256", func(r io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(r, 1024))
		sumText = string(b)
		return err
	})
	if errors.Is(err, errNotPublished) {
		return false, nil // this dashboard has no build for us; nothing to do
	}
	if err != nil {
		return false, fmt.Errorf("check: %w", err)
	}
	want := strings.Fields(sumText)
	if len(want) == 0 || len(want[0]) != 64 {
		return false, errors.New("check: malformed checksum from dashboard")
	}
	have, err := fileSHA256(exe)
	if err != nil {
		return false, err
	}
	if have == want[0] {
		return false, nil
	}
	if n := len(a.runningRunIDs()); n > 0 {
		log.Printf("auto-update: new agent available; waiting for %d running backup(s) to finish", n)
		return false, nil
	}

	// Download next to exe, so the final rename is atomic on the same file system.
	tmp := exe + ".new"
	defer os.Remove(tmp)
	err = a.api.download(ctx, "/download/"+name, func(r io.Reader) error {
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
	if err != nil {
		return false, fmt.Errorf("download: %w", err)
	}
	if got, err := fileSHA256(tmp); err != nil || got != want[0] {
		return false, errors.New("download: checksum mismatch, keeping the current agent")
	}
	// Refuse a binary that cannot even start here (wrong architecture, truncated).
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	out, err := exec.CommandContext(vctx, tmp, "version").Output()
	cancel()
	if err != nil {
		return false, fmt.Errorf("new agent does not run on this machine: %v", err)
	}
	if len(a.runningRunIDs()) > 0 { // a schedule fired while downloading
		return false, nil
	}
	if err := os.Rename(tmp, exe); err != nil {
		return false, fmt.Errorf("install: %w", err)
	}
	log.Printf("auto-update: %s -> %s", Version, strings.TrimSpace(string(out)))
	return true, nil
}

var errNotPublished = errors.New("not published")

// download GETs a public dashboard file and hands the body to fn. It uses a
// longer timeout than API calls: the agent binary is tens of megabytes.
func (c *apiClient) download(ctx context.Context, path string, fn func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	hc := &http.Client{Transport: c.http.Transport, Timeout: 15 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotPublished
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return fn(resp.Body)
}
