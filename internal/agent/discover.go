package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stylishseahorse/rclone-backup-manager/internal/proto"
)

// Auto-detection: ask Docker what runs on this machine and turn it into backup
// suggestions - MySQL/MariaDB containers to dump, volumes to copy or skip, and
// each Compose project's .env and compose files.
//
// Only names, paths and labels leave the machine. Container environments are
// read solely to see whether a root password variable exists; values are never
// sent to the dashboard.

type inspectedContainer struct {
	Name   string
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
	}
}

type inspectedVolume struct {
	Name       string
	Mountpoint string
	Labels     map[string]string
}

var systemDBs = map[string]bool{"information_schema": true, "performance_schema": true, "mysql": true, "sys": true}

func (a *Agent) docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, a.cfg.DockerPath, args...)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir()}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		env = append(env, "DOCKER_HOST="+h)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = lastMeaningfulLine(string(ee.Stderr))
		}
		if h := dbHint(msg); h != "" {
			msg = h + " (" + msg + ")"
		} else if strings.Contains(msg, "executable file not found") {
			msg = "Docker is not installed on this machine"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return out, nil
}

func engineOf(image string) string {
	img := strings.ToLower(image)
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	switch {
	case strings.HasPrefix(img, "mariadb"):
		return "mariadb"
	case strings.HasPrefix(img, "mysql"), strings.HasPrefix(img, "percona"):
		return "mysql"
	}
	return ""
}

func isCacheImage(image string) bool {
	img := strings.ToLower(image)
	for _, c := range []string{"redis", "valkey", "keydb", "memcached"} {
		if strings.Contains(img, c) {
			return true
		}
	}
	return false
}

// visible maps a host path to the path this agent sees: the same path for a
// native agent, or under a mounted root such as /host for the Docker agent.
func (a *Agent) visible(hostPath string) string {
	cands := []string{hostPath}
	for _, r := range a.scanner.Roots() {
		cands = append(cands, filepath.Join(r, hostPath))
	}
	for _, c := range cands {
		if p, err := a.scanner.Resolve(c); err == nil {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func skipBind(src string) bool {
	for _, p := range []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/etc/localtime", "/etc/timezone", "/etc/hosts", "/etc/resolv.conf"} {
		if src == p || strings.HasPrefix(src, p+"/") {
			return true
		}
	}
	return strings.HasSuffix(src, ".sock")
}

func (a *Agent) discover(ctx context.Context) proto.Discovery {
	res := proto.Discovery{Databases: []proto.DiscoveredDB{}, Volumes: []proto.DiscoveredVolume{}, Projects: []proto.DiscoveredProject{}}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	ids, err := a.docker(ctx, "ps", "-q")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Docker = true
	var containers []inspectedContainer
	if fields := strings.Fields(string(ids)); len(fields) > 0 {
		out, err := a.docker(ctx, append([]string{"inspect"}, fields...)...)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		if err := json.Unmarshal(out, &containers); err != nil {
			res.Error = "unexpected docker inspect output"
			return res
		}
	}

	vols := map[string]*proto.DiscoveredVolume{} // key: volume name or bind source
	projects := map[string]*proto.DiscoveredProject{}
	addProjectFiles := func(name, dir, configFiles string) {
		if name == "" {
			return
		}
		pr := projects[name]
		if pr == nil {
			pr = &proto.DiscoveredProject{Name: name, Files: []string{}}
			projects[name] = pr
		}
		if dir != "" && pr.Dir == "" {
			pr.Dir = a.visible(dir)
		}
		files := []string{}
		if dir != "" {
			for _, f := range []string{".env", ".env.production", ".env.local"} {
				files = append(files, filepath.Join(dir, f))
			}
		}
		for _, f := range strings.Split(configFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		for _, f := range files {
			if v := a.visible(f); v != "" && !contains(pr.Files, v) {
				pr.Files = append(pr.Files, v)
			}
		}
	}

	for _, c := range containers {
		name := strings.TrimPrefix(c.Name, "/")
		project := c.Config.Labels["com.docker.compose.project"]
		service := c.Config.Labels["com.docker.compose.service"]
		addProjectFiles(project, c.Config.Labels["com.docker.compose.project.working_dir"], c.Config.Labels["com.docker.compose.project.config_files"])

		engine := engineOf(c.Config.Image)
		if engine != "" {
			db := proto.DiscoveredDB{Container: name, Image: c.Config.Image, Engine: engine, Project: project, Service: service}
			for _, e := range c.Config.Env {
				k, v, _ := strings.Cut(e, "=")
				if (k == "MYSQL_ROOT_PASSWORD" || k == "MARIADB_ROOT_PASSWORD") && v != "" {
					db.HasRootEnv = true
				}
			}
			db.SuggestedName = name
			if project != "" && service != "" {
				db.SuggestedName = project + "-" + service
			}
			db.SuggestedName = strings.Trim(unsafeSeg.ReplaceAllString(db.SuggestedName, "-"), "-._")
			if db.HasRootEnv {
				t := a.testDump(ctx, proto.DumpConfig{Container: name, User: "root", UseContainerEnv: true})
				if t.OK {
					db.Version = t.Version
					for _, d := range t.Databases {
						if !systemDBs[d] {
							db.Databases = append(db.Databases, d)
						}
					}
				} else {
					db.ListError = t.Error
				}
			}
			res.Databases = append(res.Databases, db)
		}

		kind := "data"
		switch {
		case engine != "":
			kind = "database"
		case isCacheImage(c.Config.Image):
			kind = "cache"
		}
		for _, m := range c.Mounts {
			var key string
			v := &proto.DiscoveredVolume{Kind: kind, Project: project, Containers: []string{}, MountedAt: []string{}}
			switch m.Type {
			case "volume":
				key, v.Name, v.HostPath = "v:"+m.Name, m.Name, m.Source
			case "bind":
				if skipBind(m.Source) {
					continue
				}
				key, v.Name, v.HostPath, v.Bind = "b:"+m.Source, m.Source, m.Source, true
			default:
				continue
			}
			if engine != "" && m.Destination != "/var/lib/mysql" && !strings.HasPrefix(m.Destination, "/var/lib/mysql/") {
				v.Kind = "data" // e.g. an init-scripts mount on the DB container
			}
			if ex := vols[key]; ex != nil {
				v = ex
			} else {
				vols[key] = v
			}
			if !contains(v.Containers, name) {
				v.Containers = append(v.Containers, name)
			}
			if !contains(v.MountedAt, m.Destination) {
				v.MountedAt = append(v.MountedAt, m.Destination)
			}
			if kind == "database" && v.Kind == "database" || kind == "cache" {
				v.Kind = kind
			}
		}
	}

	// Named volumes not used by a running container (stopped stacks) still matter.
	if out, err := a.docker(ctx, "volume", "ls", "-q"); err == nil {
		var missing []string
		for _, n := range strings.Fields(string(out)) {
			if vols["v:"+n] == nil {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			if out, err := a.docker(ctx, append([]string{"volume", "inspect"}, missing...)...); err == nil {
				var list []inspectedVolume
				if json.Unmarshal(out, &list) == nil {
					for _, iv := range list {
						if len(iv.Name) == 64 && !strings.ContainsAny(iv.Name, "_-.") {
							continue // anonymous volume: a hash nobody chose
						}
						vols["v:"+iv.Name] = &proto.DiscoveredVolume{Name: iv.Name, HostPath: iv.Mountpoint, Kind: "data",
							Project: iv.Labels["com.docker.compose.project"], Containers: []string{}, MountedAt: []string{}}
					}
				}
			}
		}
	}

	for _, v := range vols {
		if v.Kind == "data" && looksLikeCache(v) {
			v.Kind = "cache"
		}
		v.Path = a.visible(v.HostPath)
		res.Volumes = append(res.Volumes, *v)
	}
	sort.Slice(res.Volumes, func(i, j int) bool {
		if res.Volumes[i].Project != res.Volumes[j].Project {
			return res.Volumes[i].Project < res.Volumes[j].Project
		}
		return res.Volumes[i].Name < res.Volumes[j].Name
	})
	for _, p := range projects {
		sort.Strings(p.Files)
		res.Projects = append(res.Projects, *p)
	}
	sort.Slice(res.Projects, func(i, j int) bool { return res.Projects[i].Name < res.Projects[j].Name })
	sort.Slice(res.Databases, func(i, j int) bool { return res.Databases[i].Container < res.Databases[j].Container })
	return res
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// looksLikeCache spots cache volumes by name or mount point, e.g. Laravel's
// app_cache at /var/www/html/bootstrap/cache: rebuildable, not worth backing up.
func looksLikeCache(v *proto.DiscoveredVolume) bool {
	if !v.Bind && strings.Contains(strings.ToLower(v.Name), "cache") {
		return true
	}
	for _, m := range v.MountedAt {
		if strings.HasSuffix(m, "/cache") || strings.Contains(m, "/cache/") {
			return true
		}
	}
	return false
}
