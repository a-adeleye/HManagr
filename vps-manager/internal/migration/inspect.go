package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// docker-compose config --format json produces a normalized stack description.
// We only decode the bits we care about; the rest is left as raw JSON.
type composeConfig struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
	Volumes  map[string]any            `json:"volumes"`
	Networks map[string]composeNetwork `json:"networks"`
}

type composeNetwork struct {
	Name     string   `json:"name"`     // actual docker network name (no project prefix when external)
	External flexBool `json:"external"` // true when compose must NOT create it
}

// flexBool decodes compose's `external`, which `config` normally renders as a
// bool but which has historically also appeared as an object ({name: ...}).
// Any present, non-false value means the network is external.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var v bool
	if err := json.Unmarshal(b, &v); err == nil {
		*f = flexBool(v)
		return nil
	}
	s := strings.TrimSpace(string(b))
	*f = flexBool(s != "" && s != "null")
	return nil
}

type composeService struct {
	Image   string          `json:"image"`
	Build   json.RawMessage `json:"build"` // present (object) when the service builds from source
	EnvFile envFiles        `json:"env_file"`
	Volumes []composeVolume `json:"volumes"`
}

func (s composeService) builds() bool {
	b := strings.TrimSpace(string(s.Build))
	return b != "" && b != "null"
}

// envFiles decodes a service's env_file, which docker compose has emitted in
// two shapes: a plain list of paths (older Compose) and, since v2.24, a list
// of {path, required} objects. We only need the paths; tolerating both keeps
// Inspect from blowing up on a JSON-decode error depending on the host's
// Compose version.
type envFiles []string

func (e *envFiles) UnmarshalJSON(b []byte) error {
	var paths []string
	if err := json.Unmarshal(b, &paths); err == nil {
		*e = paths
		return nil
	}
	var objs []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(b, &objs); err != nil {
		return err
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.Path != "" {
			out = append(out, o.Path)
		}
	}
	*e = out
	return nil
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
	seenImg := map[string]bool{}
	for name, svc := range cfg.Services {
		inv.Services = append(inv.Services, Service{
			Name:       name,
			Image:      svc.Image,
			IsPostgres: strings.Contains(strings.ToLower(svc.Image), "postgres"),
			Builds:     svc.builds(),
		})
		if svc.builds() {
			if img := resolveBuildImage(ctx, exec, cfg.Name, name, svc.Image); img != "" && !seenImg[img] {
				seenImg[img] = true
				inv.BuildImages = append(inv.BuildImages, img)
			}
		}
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
				// Binds under the compose dir ride along with the whole-directory
				// copy; only those pointing elsewhere on the host aren't migrated.
				if _, inDir := relUnder(sourcePath, v.Source); inDir {
					continue
				}
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

	// External networks must exist on the target before `up`, so collect their
	// real docker names (the `name` field, or the map key as a fallback).
	for key, net := range cfg.Networks {
		if !bool(net.External) {
			continue
		}
		name := net.Name
		if name == "" {
			name = key
		}
		inv.ExternalNetworks = append(inv.ExternalNetworks, name)
	}
	sort.Strings(inv.ExternalNetworks)

	// Best-effort size lookup — failures don't abort the inspect.
	for i, v := range inv.Volumes {
		if sz, err := volumeSize(ctx, exec, v.Name); err == nil {
			inv.Volumes[i].Size = sz
		}
	}

	if len(inv.BindMounts) > 0 {
		inv.Warnings = append(inv.Warnings,
			"bind mounts outside the stack directory are not migrated — copy these manually if they hold data")
	}
	if len(inv.ExternalNetworks) > 0 {
		inv.Warnings = append(inv.Warnings,
			"external network(s) "+strings.Join(inv.ExternalNetworks, ", ")+
				" will be created on the target if missing — their attached containers from other stacks are not migrated")
	}
	if len(inv.BuildImages) > 0 {
		inv.Warnings = append(inv.Warnings,
			"built image(s) "+strings.Join(inv.BuildImages, ", ")+
				" will be shipped prebuilt (docker save/load) so the target needn't rebuild from source")
	}
	return inv, nil
}

// resolveBuildImage works out the docker image tag a build service resolves to.
// An explicit `image:` wins; otherwise compose names the built image
// <project>-<service> (v2) or <project>_<service> (older). We probe the source
// for whichever actually exists so the tag we save/load matches what compose
// expects on the target, defaulting to the v2 form when neither is present yet.
func resolveBuildImage(ctx context.Context, exec ExecFn, project, service, explicit string) string {
	if explicit != "" {
		return explicit
	}
	hyphen := strings.ToLower(project + "-" + service)
	if imageExists(ctx, exec, hyphen) {
		return hyphen
	}
	under := strings.ToLower(project + "_" + service)
	if imageExists(ctx, exec, under) {
		return under
	}
	return hyphen
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
