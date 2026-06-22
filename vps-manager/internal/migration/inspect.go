package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ComposeContext is the compose project name + config file(s) a running stack
// actually uses, recovered from its containers' compose labels. It lets the
// migration drive `docker compose` with the correct -p/-f even when the file has
// a non-standard name, the project name differs from the directory, or multiple
// -f files (incl. override files) were used.
type ComposeContext struct {
	Project      string   `json:"project"`
	ComposeFiles []string `json:"composeFiles"`
}

// composeFlags renders `-p <project>` and one `-f` per file (each only when set),
// with a trailing space so it slots straight before the subcommand. An empty
// files slice emits no -f, so `docker compose` keeps its native default-file +
// override-file auto-merge.
func composeFlags(project string, files []string) string {
	var b strings.Builder
	if p := strings.TrimSpace(project); p != "" {
		b.WriteString("-p " + shellQuote(p) + " ")
	}
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			b.WriteString("-f " + shellQuote(f) + " ")
		}
	}
	return b.String()
}

// DiscoverComposeContext reads the compose project + config files a running stack
// uses from its containers' labels (com.docker.compose.project[.config_files]),
// matched by working_dir. It keeps ALL config files (so override files and
// multi-`-f` setups migrate faithfully) and resolves symlinked/non-canonical
// source paths. Returns zero values when nothing matches (the stack isn't
// running or wasn't started by compose).
func DiscoverComposeContext(ctx context.Context, exec ExecFn, dir string) ComposeContext {
	d := strings.TrimRight(strings.TrimSpace(dir), "/")
	// Compose stores the canonical absolute working_dir; resolve the user's path
	// so a symlink/relative form still matches the label.
	if res, err := exec(ctx, "readlink -f "+shellQuote(d)); err == nil && res != nil && res.ExitCode == 0 {
		if rp := strings.TrimSpace(res.Stdout); rp != "" {
			d = strings.TrimRight(rp, "/")
		}
	}
	format := `{{.Label "com.docker.compose.project"}}` + "\t" + `{{.Label "com.docker.compose.project.config_files"}}`
	cmd := "docker ps -a --filter " + shellQuote("label=com.docker.compose.project.working_dir="+d) + " --format " + shellQuote(format)
	res, err := exec(ctx, cmd)
	if err != nil || res == nil || res.ExitCode != 0 {
		return ComposeContext{}
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		proj := strings.TrimSpace(parts[0])
		var files []string
		for _, fp := range strings.Split(parts[1], ",") {
			fp = strings.TrimSpace(fp)
			if fp == "" {
				continue
			}
			if rel, ok := relUnder(d, fp); ok {
				files = append(files, rel)
			} else {
				files = append(files, fp) // out-of-dir compose file: keep absolute -f
			}
		}
		if proj != "" || len(files) > 0 {
			return ComposeContext{Project: proj, ComposeFiles: files}
		}
	}
	return ComposeContext{}
}

// docker-compose config --format json produces a normalized stack description.
// We only decode the bits we care about; the rest is left as raw JSON.
type composeConfig struct {
	Name     string                      `json:"name"`
	Services map[string]composeService   `json:"services"`
	Volumes  map[string]composeVolumeDef `json:"volumes"`
	Networks map[string]composeNetwork   `json:"networks"`
}

// composeVolumeDef is a top-level volume declaration. External volumes aren't
// created by compose (no <project>_ prefix) and keep their real name.
type composeVolumeDef struct {
	Name     string   `json:"name"`
	External flexBool `json:"external"`
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
func Inspect(ctx context.Context, exec ExecFn, sourcePath string, composeFiles []string, projectName string) (*Inventory, error) {
	cmd := fmt.Sprintf("cd %s && docker compose %sconfig --format json", shellQuote(sourcePath), composeFlags(projectName, composeFiles))
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
			// A build service's image is produced locally — always ship it.
			if img := resolveBuildImage(ctx, exec, cfg.Name, name, svc.Image); img != "" && !seenImg[img] {
				seenImg[img] = true
				inv.BuildImages = append(inv.BuildImages, img)
			}
		} else if svc.Image != "" && !seenImg[svc.Image] && isLocalOnlyImage(ctx, exec, svc.Image) {
			// A non-build service that references an image which is present on the
			// source but NOT pullable from a registry (e.g. a CI-built tag like
			// app:<gitsha>) — the target can't pull it, so ship it too.
			seenImg[svc.Image] = true
			inv.BuildImages = append(inv.BuildImages, svc.Image)
		}
		for _, v := range svc.Volumes {
			switch v.Type {
			case "volume":
				// Declared, non-external volumes get the project-name prefix when
				// docker creates them. External volumes keep their real name (the
				// `name:` field, or the key); anonymous ones pass through as-is.
				full := v.Source
				if def, declared := cfg.Volumes[v.Source]; declared {
					if bool(def.External) {
						if def.Name != "" {
							full = def.Name
						}
					} else {
						full = cfg.Name + "_" + v.Source
					}
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
			"image(s) not available from a registry ("+strings.Join(inv.BuildImages, ", ")+
				") will be copied from the source (docker save/load) so the target needn't pull or rebuild them")
	}
	return inv, nil
}

// isLocalOnlyImage reports whether image is present on the source but cannot be
// pulled from a registry — i.e. a locally-built / CI-tagged image the target
// would fail to pull. Registry images (postgres:16, …) return false so the
// target just pulls them. If `docker manifest inspect` isn't supported, we
// assume the image is pullable rather than mass-shipping every public image.
func isLocalOnlyImage(ctx context.Context, exec ExecFn, image string) bool {
	if !imageExists(ctx, exec, image) {
		return false // not on the source; nothing to ship (target will pull)
	}
	res, err := exec(ctx, "docker manifest inspect "+shellQuote(image))
	if err != nil || res == nil {
		return false
	}
	if res.ExitCode == 0 {
		return false // pullable from a registry
	}
	out := strings.ToLower(res.Stderr + res.Stdout)
	if strings.Contains(out, "not a docker command") || strings.Contains(out, "is not a docker") {
		return false // old docker without `manifest` — don't assume local-only
	}
	return true // present locally, not pullable → ship it
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

// DiscoverStacks scans the immediate subdirectories of dir for compose files and
// returns the ones that look like stacks. Used when dir itself has no compose
// file — i.e. it's a parent folder holding several stacks. One level deep only,
// so example/vendored compose files nested in build contexts aren't matched.
func DiscoverStacks(ctx context.Context, exec ExecFn, dir string) ([]SubStack, error) {
	// For each immediate subdir, print "<name>\t<composefile>" for the first
	// compose file it contains.
	script := "cd " + shellQuote(dir) + ` 2>/dev/null || exit 0
for d in */; do
  d="${d%/}"
  for c in compose.yaml compose.yml docker-compose.yaml docker-compose.yml; do
    if [ -f "$d/$c" ]; then printf '%s\t%s\n' "$d" "$c"; break; fi
  done
done`
	res, err := exec(ctx, script)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(dir, "/")
	var out []SubStack
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		out = append(out, SubStack{Name: parts[0], Path: base + "/" + parts[0], ComposeFile: parts[1]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
