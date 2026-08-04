// Package ai exposes a small set of READ-ONLY diagnostic tools to an AI agent
// (Codex CLI, driving the app's app.go AskAI flow) over MCP, so the model can
// investigate a connected VPS without ever touching its SSH credentials. The
// server is bound to one already-authenticated exec function for the
// lifetime of a single query — the same execFn every other tab in this app
// uses — so the model's reach is exactly the diagnostic surface defined here,
// nothing more.
//
// There is deliberately no delete/prune/write tool in this package. "Redundant
// images and volumes" is a question the model can fully answer read-only;
// acting on the answer stays a human decision made through the existing
// Maintenance tab.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a remote/local command — the same shape used by the docker, db,
// maintenance and stats packages, so App.execFn feeds this one too.
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// NewServer builds an MCP server exposing read-only Docker diagnostic tools
// backed by exec. Each tool call re-runs its command fresh — no caching —
// since the model may act on stale state otherwise.
func NewServer(exec ExecFn) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "vps-manager", Version: "1.0.0"}, nil)

	// Every tool here is read-only by construction (see internal/ai's package
	// doc), so ReadOnlyHint is set truthfully on all of them — not just to be
	// informative, but because Codex's non-interactive `exec` mode gates any
	// tool call missing this hint behind a trust prompt it can't answer
	// (default DestructiveHint is true per the MCP spec), and there is no
	// terminal to answer it from in that mode. Without the hint, every call
	// here would fail with "user cancelled MCP tool call".
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_docker_images",
		Description: "List Docker images on the server, each flagged dangling (untagged, always " +
			"safe to remove) or inUse=false (tagged but not referenced by any container — a " +
			"candidate for removal, but confirm with the user before deleting anything).",
		Annotations: readOnly,
	}, toolFn(exec, listImages))

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_docker_volumes",
		Description: "List Docker volumes on the server, each flagged inUse=false when no " +
			"container (running or stopped) mounts it — an orphaned volume, safe to remove if " +
			"its data isn't needed elsewhere.",
		Annotations: readOnly,
	}, toolFn(exec, listVolumes))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_containers",
		Description: "List all containers (running and stopped) with their state, image and compose project/service — context for why an image or volume might still be in use.",
		Annotations: readOnly,
	}, toolFn(exec, listContainers))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "docker_disk_usage",
		Description: "Summarize Docker's disk usage by category (images, containers, volumes, build cache) — output of `docker system df`, including how much of each is reclaimable.",
		Annotations: readOnly,
	}, toolFn(exec, diskUsage))

	mcp.AddTool(s, &mcp.Tool{
		Name: "run_readonly_command",
		Description: "Run one read-only shell command on the server for anything the other tools " +
			"don't cover — e.g. `docker inspect <id>`, `du -sh /var/lib/docker/volumes/*`, " +
			"`docker network ls`. Only inspection/listing commands are allowed (docker ps/images/" +
			"volume/network/system/inspect/logs, df, du, cat, ls, free, uptime, nproc); anything " +
			"else — writes, deletes, pipes to a shell, redirections — is rejected before it runs.",
		Annotations: readOnly,
	}, toolFn(exec, runReadonly))

	return s
}

// toolFn adapts a (ExecFn, In) -> (string, error) pair into the SDK's
// generic ToolHandlerFor, wrapping the result as plain text content. Every
// tool in this package returns human/model-readable text (JSON or a table),
// not structured output — the model reads it the same way a person reading
// terminal output would.
func toolFn[In any](exec ExecFn, fn func(context.Context, ExecFn, In) (string, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		text, err := fn(ctx, exec, in)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}
}

// ───────── list_docker_images ─────────

type imageRow struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	ID         string `json:"id"`
	Size       string `json:"size"`
	Dangling   bool   `json:"dangling"`
	InUse      bool   `json:"inUse"`
}

type dockerImagesLine struct {
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	ID         string `json:"ID"`
	Size       string `json:"Size"`
}

func listImages(ctx context.Context, exec ExecFn, _ struct{}) (string, error) {
	res, err := exec(ctx, `docker images -a --format '{{json .}}'`)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", dockerErr("docker images", res)
	}
	inUseImages, err := imagesInUse(ctx, exec)
	if err != nil {
		return "", err
	}
	return renderImages(res.Stdout, inUseImages), nil
}

// renderImages is the pure parser: docker images JSON lines + the set of
// image refs currently used by a container -> the tool's JSON text output.
func renderImages(out string, inUse map[string]bool) string {
	var rows []imageRow
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var raw dockerImagesLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		dangling := raw.Repository == "<none>" || raw.Tag == "<none>"
		ref := raw.Repository + ":" + raw.Tag
		rows = append(rows, imageRow{
			Repository: raw.Repository,
			Tag:        raw.Tag,
			ID:         raw.ID,
			Size:       raw.Size,
			Dangling:   dangling,
			InUse:      !dangling && (inUse[ref] || inUse[raw.ID]),
		})
	}
	return mustJSON(rows)
}

// imagesInUse returns the set of image references (repo:tag and short ID)
// that at least one container — running or stopped — currently points at.
func imagesInUse(ctx context.Context, exec ExecFn) (map[string]bool, error) {
	res, err := exec(ctx, `docker ps -a --format '{{.Image}}'`)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, dockerErr("docker ps", res)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[line] = true
		if !strings.Contains(line, ":") {
			set[line+":latest"] = true
		}
	}
	return set, nil
}

// ───────── list_docker_volumes ─────────

type volumeRow struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Mountpoint string `json:"mountpoint"`
	InUse      bool   `json:"inUse"`
}

func listVolumes(ctx context.Context, exec ExecFn, _ struct{}) (string, error) {
	res, err := exec(ctx, `docker volume ls --format '{{json .}}'`)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", dockerErr("docker volume ls", res)
	}
	inUseRes, err := exec(ctx, `docker volume ls -f dangling=false --format '{{.Name}}'`)
	if err != nil {
		return "", err
	}
	inUse := map[string]bool{}
	if inUseRes.ExitCode == 0 {
		for _, name := range strings.Split(strings.TrimSpace(inUseRes.Stdout), "\n") {
			if name != "" {
				inUse[name] = true
			}
		}
	}
	return renderVolumes(res.Stdout, inUse), nil
}

type dockerVolumeLine struct {
	Driver     string `json:"Driver"`
	Name       string `json:"Name"`
	Mountpoint string `json:"Mountpoint"`
}

func renderVolumes(out string, inUse map[string]bool) string {
	var rows []volumeRow
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var raw dockerVolumeLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		rows = append(rows, volumeRow{
			Name:       raw.Name,
			Driver:     raw.Driver,
			Mountpoint: raw.Mountpoint,
			InUse:      inUse[raw.Name],
		})
	}
	return mustJSON(rows)
}

// ───────── list_containers ─────────

type containerRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Service string `json:"service,omitempty"`
}

type dockerPSLine struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Labels string `json:"Labels"`
}

func listContainers(ctx context.Context, exec ExecFn, _ struct{}) (string, error) {
	res, err := exec(ctx, `docker ps -a --format '{{json .}}'`)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", dockerErr("docker ps", res)
	}
	var rows []containerRow
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		var raw dockerPSLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		rows = append(rows, containerRow{
			ID: raw.ID, Name: raw.Names, Image: raw.Image, State: raw.State, Status: raw.Status,
			Project: labelValue(raw.Labels, "com.docker.compose.project"),
			Service: labelValue(raw.Labels, "com.docker.compose.service"),
		})
	}
	return mustJSON(rows), nil
}

func labelValue(labels, key string) string {
	for _, part := range strings.Split(labels, ",") {
		if strings.HasPrefix(part, key+"=") {
			return part[len(key)+1:]
		}
	}
	return ""
}

// ───────── docker_disk_usage ─────────

func diskUsage(ctx context.Context, exec ExecFn, _ struct{}) (string, error) {
	res, err := exec(ctx, "docker system df")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", dockerErr("docker system df", res)
	}
	return res.Stdout, nil
}

// ───────── run_readonly_command ─────────

type readonlyIn struct {
	Command string `json:"command" jsonschema:"the read-only command to run, e.g. 'docker inspect abc123'"`
}

// readonlyPrefixes are the only command starts allowed through
// run_readonly_command. Deliberately narrow: listing/inspecting Docker state
// plus a handful of standard read-only system utilities.
var readonlyPrefixes = []string{
	"docker ps", "docker images", "docker image ls", "docker image inspect",
	"docker volume", "docker network", "docker inspect", "docker logs",
	"docker system df", "docker system info", "docker version", "docker stats --no-stream",
	"df ", "df", "du ", "cat ", "ls ", "ls", "free", "uptime", "nproc",
}

// forbiddenChars blocks shell chaining/redirection/substitution, so a
// command that passes the prefix check still can't smuggle a second,
// unvetted command alongside it.
const forbiddenChars = ";|&`$><\n"

func runReadonly(ctx context.Context, exec ExecFn, in readonlyIn) (string, error) {
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return "", fmt.Errorf("command is empty")
	}
	if strings.ContainsAny(cmd, forbiddenChars) {
		return "", fmt.Errorf("rejected: command contains a shell metacharacter (;|&`$><) — one plain read-only command only, no chaining or redirection")
	}
	allowed := false
	for _, p := range readonlyPrefixes {
		if strings.HasPrefix(cmd, p) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("rejected: %q is not on the read-only allowlist (docker ps/images/volume/network/inspect/logs/system, df, du, cat, ls, free, uptime, nproc)", cmd)
	}
	res, err := exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	out := res.Stdout
	if res.ExitCode != 0 {
		out += "\n[exit " + strconv.Itoa(res.ExitCode) + "] " + res.Stderr
	}
	return out, nil
}

// ───────── helpers ─────────

func dockerErr(what string, res *sshpkg.ExecResult) error {
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return fmt.Errorf("%s: %s", what, msg)
}

func boolPtr(b bool) *bool { return &b }

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}
