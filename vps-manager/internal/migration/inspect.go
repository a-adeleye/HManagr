package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// docker-compose config --format json produces a normalized stack description.
// We only decode the bits we care about; the rest is left as raw JSON.
type composeConfig struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
	Volumes  map[string]any            `json:"volumes"`
}

type composeService struct {
	Image   string          `json:"image"`
	EnvFile []string        `json:"env_file"`
	Volumes []composeVolume `json:"volumes"`
}

type composeVolume struct {
	Type   string `json:"type"`   // "volume", "bind", or "tmpfs"
	Source string `json:"source"` // base volume name (type=volume) or host path (type=bind)
	Target string `json:"target"`
}

// Inspect runs `docker compose config --format json` at sourcePath via exec
// and returns the resolved inventory the wizard will show. Taking ExecFn (not
// *sshpkg.Connection) lets the App layer inject a sudo-wrapped exec when the
// user opts into that.
func Inspect(ctx context.Context, exec ExecFn, sourcePath string) (*Inventory, error) {
	cmd := fmt.Sprintf("cd %s && docker compose config --format json", shellQuote(sourcePath))
	res, err := exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		if stderr == "" {
			stderr = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return nil, fmt.Errorf("docker compose config: %s", stderr)
	}

	var cfg composeConfig
	if err := json.Unmarshal([]byte(res.Stdout), &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}

	inv := &Inventory{ProjectName: cfg.Name}

	seenVol := map[string]bool{}
	seenBind := map[string]bool{}
	for name, svc := range cfg.Services {
		inv.Services = append(inv.Services, Service{
			Name:       name,
			Image:      svc.Image,
			IsPostgres: strings.Contains(strings.ToLower(svc.Image), "postgres"),
		})
		for _, v := range svc.Volumes {
			switch v.Type {
			case "volume":
				// Declared volumes get the project-name prefix when docker
				// creates them. External / anonymous volumes don't.
				full := v.Source
				if _, declared := cfg.Volumes[v.Source]; declared {
					full = cfg.Name + "_" + v.Source
				}
				if !seenVol[full] {
					seenVol[full] = true
					inv.Volumes = append(inv.Volumes, Volume{Name: full})
				}
			case "bind":
				if !seenBind[v.Source] {
					seenBind[v.Source] = true
					inv.BindMounts = append(inv.BindMounts, v.Source)
				}
			}
		}
	}

	envSet := map[string]bool{}
	for _, svc := range cfg.Services {
		for _, ef := range svc.EnvFile {
			if !envSet[ef] {
				envSet[ef] = true
				inv.EnvFiles = append(inv.EnvFiles, ef)
			}
		}
	}

	// Best-effort size lookup — failures don't abort the inspect.
	for i, v := range inv.Volumes {
		if sz, err := volumeSize(ctx, exec, v.Name); err == nil {
			inv.Volumes[i].Size = sz
		}
	}

	if len(inv.BindMounts) > 0 {
		inv.Warnings = append(inv.Warnings,
			"bind mounts are not migrated in v1 — copy these manually if they hold data")
	}
	return inv, nil
}

// volumeSize asks alpine inside the volume to report its size, sidestepping
// the host's root-only access to /var/lib/docker/volumes/*/_data.
func volumeSize(ctx context.Context, exec ExecFn, name string) (int64, error) {
	cmd := fmt.Sprintf("docker run --rm -v %s:/data:ro alpine sh -c 'du -sb /data | cut -f1'", shellQuote(name))
	res, err := exec(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return 0, fmt.Errorf("volume size lookup failed")
	}
	var n int64
	_, err = fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &n)
	return n, err
}

// FindComposeFile picks the first known compose filename that exists in dir.
// It is a small convenience the App layer calls before Run so the user doesn't
// have to type the filename themselves.
func FindComposeFile(ctx context.Context, exec ExecFn, dir string) (string, error) {
	candidates := []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	for _, name := range candidates {
		cmd := fmt.Sprintf("test -f %s/%s", shellQuote(dir), shellQuote(name))
		if res, err := exec(ctx, cmd); err == nil && res.ExitCode == 0 {
			return name, nil
		}
	}
	return "", fmt.Errorf("no compose file found in %s", dir)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
