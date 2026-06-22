// Package caddy gives a one-click "expose this service on a domain with
// automatic HTTPS" flow, shaped to the user's actual setup rather than imposing
// a new reverse proxy.
//
// The user runs caddy-docker-proxy (lucaslorentz) — a label-driven Caddy that
// watches the docker socket and configures itself from per-service labels. It
// already owns :80/:443. Standing up a second, Caddyfile-driven Caddy would make
// two processes fight over those ports, so the PRIMARY mechanism here is LABELS,
// not a Caddyfile:
//
//   - DetectProxy scans `docker ps` for a running caddy-docker-proxy and reports
//     the docker network it is attached to (a service must share that network to
//     be routed to).
//   - RenderOverride produces a compose OVERRIDE file that adds the routing
//     labels (caddy / caddy.reverse_proxy) to the target service. We return an
//     override merged via an extra `-f` instead of editing the user's compose in
//     place: the user's file is never rewritten (no risk of mangling it, trivial
//     to revert by dropping the `-f`), and compose's native last-file-wins merge
//     layers our labels on cleanly. ApplyOverride writes it next to the stack and
//     re-ups.
//
// A Caddyfile FALLBACK (RenderCaddyfile / InstallCaddyfile) exists only for the
// SECONDARY case of a host with no caddy-docker-proxy at all — there a plain
// Caddy reverse_proxy snippet is the simplest thing that works.
//
// Cloudflare DNS is handled by CloudflareUpsertA, which talks to the Cloudflare
// API directly over HTTPS FROM THE DESKTOP. The API token never touches the VPS
// — it is not interpolated into any remote command — so it can't leak via `ps`,
// shell history, or sudo's syslog. That function is the only thing in this
// package that does network I/O off-box; everything else operates through the
// injected ExecFn/StreamFn so the caller controls (and can sudo-wrap) execution.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a buffered remote command (possibly sudo-wrapped by the caller).
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a remote command and streams combined output as it arrives.
// Returns the exit code.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// ───────── proxy detection ─────────

// ProxyInfo describes a caddy-docker-proxy found running on the host. Network is
// the docker network the proxy is attached to; a target service must join that
// same network for the proxy to reach it. It is best-effort and may be empty.
type ProxyInfo struct {
	Present   bool   `json:"present"`
	Container string `json:"container"`
	Image     string `json:"image"`
	Network   string `json:"network"`
}

// DetectProxy reports whether a caddy-docker-proxy is already running on the
// host. It scans `docker ps` for an image containing "caddy-docker-proxy"
// (lucaslorentz's label-driven Caddy), falling back to any container whose image
// is plain "caddy" — those are usually the same proxy under a renamed image. The
// attached docker network is looked up with `docker inspect` (best-effort).
func DetectProxy(ctx context.Context, exec ExecFn) (*ProxyInfo, error) {
	res, err := exec(ctx, "docker ps --format '{{.Names}}\t{{.Image}}'")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps: %s", strings.TrimSpace(res.Stderr))
	}
	info := parseProxyPS(res.Stdout)
	if !info.Present {
		return info, nil
	}
	info.Network = inspectProxyNetwork(ctx, exec, info.Container)
	return info, nil
}

// parseProxyPS picks the proxy out of `docker ps` "<name>\t<image>" lines. An
// image containing "caddy-docker-proxy" wins outright; otherwise the first
// container whose image name looks like Caddy is taken as a likely proxy. Pure
// (no exec) so it can be unit-tested.
func parseProxyPS(out string) *ProxyInfo {
	var fallback *ProxyInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		image := strings.TrimSpace(parts[1])
		img := strings.ToLower(image)
		if strings.Contains(img, "caddy-docker-proxy") {
			return &ProxyInfo{Present: true, Container: name, Image: image}
		}
		if fallback == nil && imageLooksLikeCaddy(img) {
			fallback = &ProxyInfo{Present: true, Container: name, Image: image}
		}
	}
	if fallback != nil {
		return fallback
	}
	return &ProxyInfo{}
}

// imageLooksLikeCaddy reports whether an image reference is (probably) a Caddy
// image: its repository path's final component is exactly "caddy" (so "caddy",
// "library/caddy:2", "lucaslorentz/caddy-docker-proxy" all match, but
// "mycaddyshack/app" does not).
func imageLooksLikeCaddy(img string) bool {
	repo := img
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndexByte(repo, ':'); i >= 0 {
		repo = repo[:i]
	}
	repo = strings.Trim(repo, "/")
	last := repo
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		last = repo[i+1:]
	}
	return last == "caddy" || strings.Contains(last, "caddy-docker-proxy")
}

// inspectProxyNetwork returns the first non-default docker network the proxy is
// attached to (best-effort; "" when it can't be determined). Compose-created
// networks like "bridge"/"host"/"none" are skipped so the user gets the real
// shared network name to attach services to.
func inspectProxyNetwork(ctx context.Context, exec ExecFn, container string) string {
	format := "{{range $k, $v := .NetworkSettings.Networks}}{{$k}}\n{{end}}"
	res, err := exec(ctx, "docker inspect -f "+shellQuote(format)+" "+shellQuote(container))
	if err != nil || res == nil || res.ExitCode != 0 {
		return ""
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		net := strings.TrimSpace(line)
		switch net {
		case "", "bridge", "host", "none":
			continue
		}
		return net
	}
	return ""
}

// ───────── label override (the primary mechanism) ─────────

// ExposeSpec describes one "expose service on domain(s)" request. Service is the
// compose service key the labels are attached to; Domains are the hostnames it
// should answer on; Port is the container port the proxy forwards to. MainCompose
// is the user's existing compose file (relative to StackPath or absolute), used
// as the first `-f`. Network, when set, also attaches the service to that docker
// network (the proxy's network from DetectProxy) so the proxy can reach it.
type ExposeSpec struct {
	StackPath    string   `json:"stackPath"`    // compose dir on the VPS
	MainCompose  string   `json:"mainCompose"`  // existing compose file name/path
	Service      string   `json:"service"`      // compose service key
	Domains      []string `json:"domains"`      // hostnames to route
	Port         int      `json:"port"`         // upstream container port
	Network      string   `json:"network"`      // proxy network to attach (optional)
	OverrideName string   `json:"overrideName"` // override filename (default below)
}

// defaultOverrideName is the override file we write next to the stack. It is
// distinctive so it's obvious it was generated and safe to delete.
const defaultOverrideName = "vps-manager-caddy.override.yml"

// validDomainRe-equivalent: domains and service/network names are validated with
// an allowlist before being interpolated, mirroring backup.validID. We don't
// regex-match a full RFC hostname — just reject anything outside the safe charset
// so a value can never break out of the YAML/command it lands in.
func validDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '*' || r == '_') {
			return false
		}
	}
	return true
}

// validName allows a docker/compose identifier (service key, network name).
func validName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// validateSpec checks every interpolated value against an allowlist and the port
// range, returning a clear error so a bad request never reaches a rendered file.
func validateSpec(spec ExposeSpec) error {
	if !validName(spec.Service) {
		return fmt.Errorf("invalid service name %q", spec.Service)
	}
	if len(spec.Domains) == 0 {
		return fmt.Errorf("at least one domain is required")
	}
	for _, d := range spec.Domains {
		if !validDomain(d) {
			return fmt.Errorf("invalid domain %q", d)
		}
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return fmt.Errorf("invalid upstream port %d", spec.Port)
	}
	if spec.Network != "" && !validName(spec.Network) {
		return fmt.Errorf("invalid network %q", spec.Network)
	}
	return nil
}

// RenderOverride produces a compose override file that adds caddy-docker-proxy
// routing labels to spec.Service. The labels are:
//
//	caddy: "<domain>[ <domain>…]"
//	caddy.reverse_proxy: "{{upstreams <port>}}"
//
// caddy-docker-proxy reads `caddy` as the site address(es) and turns the
// `{{upstreams <port>}}` template into the in-network upstream for this service,
// so we never hardcode an IP. When spec.Network is set the service is also joined
// to that (external) network so the proxy can reach it. Validate spec with
// validateSpec before calling; callers go through ApplyOverride which does.
func RenderOverride(spec ExposeSpec) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }

	w("# Generated by vps-manager — adds caddy-docker-proxy routing labels.")
	w("# Merged via an extra `-f`; your original compose file is left untouched.")
	w("# Remove this file (and its -f) to stop exposing the service.")
	w("services:")
	w("  " + spec.Service + ":")
	w("    labels:")
	w(`      caddy: ` + yamlQuote(strings.Join(spec.Domains, " ")))
	w(`      caddy.reverse_proxy: ` + yamlQuote(fmt.Sprintf("{{upstreams %d}}", spec.Port)))
	if spec.Network != "" {
		w("    networks:")
		w("      - " + spec.Network)
		w("networks:")
		w("  " + spec.Network + ":")
		w("    external: true")
	}
	return b.String()
}

// yamlQuote double-quotes a scalar for YAML, escaping backslashes and quotes.
// Domains/ports pass validateSpec first, so this is belt-and-braces.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// overrideName returns the spec's override filename or the default.
func overrideName(spec ExposeSpec) string {
	if n := strings.TrimSpace(spec.OverrideName); n != "" {
		return n
	}
	return defaultOverrideName
}

// ApplyOverride writes the rendered override next to the stack and re-ups the
// project with both files: `docker compose -f <main> -f <override> up -d`. Output
// is streamed to onOutput. The main compose file is supplied by the caller
// (DiscoverComposeContext / FindComposeFile upstream) so we don't second-guess
// its name. Returns an error if validation, the write, or the up fails.
func ApplyOverride(ctx context.Context, exec ExecFn, stream StreamFn, spec ExposeSpec, onOutput func(string)) error {
	if onOutput == nil {
		onOutput = func(string) {}
	}
	if err := validateSpec(spec); err != nil {
		return err
	}
	if strings.TrimSpace(spec.StackPath) == "" {
		return fmt.Errorf("stack path is required")
	}
	main := strings.TrimSpace(spec.MainCompose)
	if main == "" {
		return fmt.Errorf("main compose file is required")
	}

	override := overrideName(spec)
	overridePath := path.Join(spec.StackPath, override)
	content := RenderOverride(spec)

	onOutput("→ Writing routing override " + override + "…")
	if err := writeFile(ctx, exec, overridePath, content, "644"); err != nil {
		return fmt.Errorf("write override: %w", err)
	}

	onOutput(fmt.Sprintf("→ Re-upping stack (docker compose -f %s -f %s up -d)…", main, override))
	up := fmt.Sprintf("cd %s && docker compose -f %s -f %s up -d --remove-orphans",
		shellQuote(spec.StackPath), shellQuote(main), shellQuote(override))
	exit, err := stream(ctx, up, func(chunk string) {
		onOutput(strings.TrimRight(chunk, "\n"))
	})
	if err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("docker compose up exited with code %d — see log above", exit)
	}
	onOutput("✓ Service exposed. Caddy will obtain a certificate on first request.")
	return nil
}

// ───────── Caddyfile fallback (secondary) ─────────

// Route is one domain → host:port mapping for the Caddyfile fallback used on
// hosts with no caddy-docker-proxy. Upstream is the address Caddy proxies to
// (e.g. "localhost:8080" or "127.0.0.1:3000").
type Route struct {
	Domain   string `json:"domain"`
	Upstream string `json:"upstream"`
}

// RenderCaddyfile renders a Caddyfile snippet — one site block per route:
//
//	example.com {
//	    reverse_proxy localhost:8080
//	}
//
// This is the SECONDARY path: only for a host running a plain, Caddyfile-driven
// Caddy (no caddy-docker-proxy). Routes with an invalid domain or empty upstream
// are skipped so a bad entry can't corrupt the file.
func RenderCaddyfile(routes []Route) string {
	var b strings.Builder
	for _, r := range routes {
		domain := strings.TrimSpace(r.Domain)
		upstream := strings.TrimSpace(r.Upstream)
		if !validDomain(domain) || upstream == "" {
			continue
		}
		b.WriteString(domain + " {\n")
		b.WriteString("\treverse_proxy " + upstream + "\n")
		b.WriteString("}\n")
	}
	return b.String()
}

// InstallCaddyfile writes the snippet to a drop-in path and best-effort reloads
// Caddy (the proxy reads the config and obtains certs). The reload failing is not
// fatal — the file is written either way, and the user may reload manually. This
// is the fallback path; ApplyOverride is preferred when a proxy is present.
func InstallCaddyfile(ctx context.Context, exec ExecFn, snippetPath string, routes []Route, onOutput func(string)) error {
	if onOutput == nil {
		onOutput = func(string) {}
	}
	if strings.TrimSpace(snippetPath) == "" {
		return fmt.Errorf("snippet path is required")
	}
	content := RenderCaddyfile(routes)
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("no valid routes to install")
	}
	onOutput("→ Writing Caddyfile snippet to " + snippetPath + "…")
	if err := writeFile(ctx, exec, snippetPath, content, "644"); err != nil {
		return fmt.Errorf("write caddyfile: %w", err)
	}
	// Best-effort reload: try `caddy reload`, then a systemd reload. Neither
	// failing aborts — the file is already in place.
	onOutput("→ Reloading Caddy (best-effort)…")
	reload := "caddy reload --config " + shellQuote(snippetPath) +
		" 2>/dev/null || systemctl reload caddy 2>/dev/null || true"
	if res, err := exec(ctx, reload); err == nil && res != nil && strings.TrimSpace(res.Stdout) != "" {
		onOutput(strings.TrimRight(res.Stdout, "\n"))
	}
	onOutput("✓ Caddyfile installed.")
	return nil
}

// ───────── shared remote helpers ─────────

// writeFile writes content to a (possibly root-owned) path with the given octal
// mode, creating parent dirs. Routed through exec so a sudo-wrapped exec lands
// root-owned destinations correctly. Mirrors backup.writeFile.
func writeFile(ctx context.Context, exec ExecFn, p, content, mode string) error {
	cmd := fmt.Sprintf("mkdir -p %s && printf '%%s' %s > %s && chmod %s %s",
		shellQuote(path.Dir(p)), shellQuote(content), shellQuote(p), mode, shellQuote(p))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// ───────── Cloudflare DNS (off-box, direct HTTPS) ─────────

const cloudflareAPIBase = "https://api.cloudflare.com/client/v4"

// dnsRecordBody is the JSON body for creating/updating an A record.
type dnsRecordBody struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// cfListResponse is the shared shape of the Cloudflare list endpoints we read
// (zones, dns_records). We only need the id of the first result.
type cfListResponse struct {
	Success bool        `json:"success"`
	Errors  []cfAPIErr  `json:"errors"`
	Result  []cfIDEntry `json:"result"`
}

// cfWriteResponse is the shape of a create/update response (single object).
type cfWriteResponse struct {
	Success bool       `json:"success"`
	Errors  []cfAPIErr `json:"errors"`
	Result  cfIDEntry  `json:"result"`
}

type cfIDEntry struct {
	ID string `json:"id"`
}

type cfAPIErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// zonesURL builds the GET zones?name= URL. Pure so it can be asserted on without
// a network call.
func zonesURL(zone string) string {
	q := url.Values{}
	q.Set("name", zone)
	return cloudflareAPIBase + "/zones?" + q.Encode()
}

// dnsRecordsURL builds the GET dns_records lookup URL for an A record by name.
func dnsRecordsURL(zoneID, name string) string {
	q := url.Values{}
	q.Set("type", "A")
	q.Set("name", name)
	return cloudflareAPIBase + "/zones/" + zoneID + "/dns_records?" + q.Encode()
}

// dnsRecordCreateURL is where a new A record is POSTed.
func dnsRecordCreateURL(zoneID string) string {
	return cloudflareAPIBase + "/zones/" + zoneID + "/dns_records"
}

// dnsRecordUpdateURL is where an existing A record is PUT.
func dnsRecordUpdateURL(zoneID, recordID string) string {
	return cloudflareAPIBase + "/zones/" + zoneID + "/dns_records/" + recordID
}

// buildRecordBody marshals the A-record body. Pure (returns the exact bytes) so
// a test can assert the JSON without making a request.
func buildRecordBody(name, ipv4 string, proxied bool) ([]byte, error) {
	return json.Marshal(dnsRecordBody{
		Type:    "A",
		Name:    name,
		Content: ipv4,
		TTL:     1, // 1 = "automatic" in the Cloudflare API
		Proxied: proxied,
	})
}

// CloudflareUpsertA creates or updates an A record from the DESKTOP via direct
// HTTPS — the token is sent only in the Authorization header to Cloudflare and
// never touches the VPS. Flow: look up the zone id by name, look up an existing A
// record by name, then PUT (update) if one exists or POST (create) if not. Errors
// from non-success responses are surfaced with Cloudflare's message.
func CloudflareUpsertA(ctx context.Context, apiToken, zone, name, ipv4 string, proxied bool) error {
	if strings.TrimSpace(apiToken) == "" {
		return fmt.Errorf("cloudflare api token is required")
	}
	if strings.TrimSpace(zone) == "" || strings.TrimSpace(name) == "" {
		return fmt.Errorf("zone and record name are required")
	}
	if strings.TrimSpace(ipv4) == "" {
		return fmt.Errorf("ipv4 address is required")
	}
	client := &http.Client{Timeout: 20 * time.Second}

	// 1. Resolve the zone id.
	zoneID, err := cfFirstID(ctx, client, apiToken, zonesURL(zone))
	if err != nil {
		return fmt.Errorf("look up zone %q: %w", zone, err)
	}
	if zoneID == "" {
		return fmt.Errorf("zone %q not found for this token", zone)
	}

	// 2. Find an existing A record for name.
	recordID, err := cfFirstID(ctx, client, apiToken, dnsRecordsURL(zoneID, name))
	if err != nil {
		return fmt.Errorf("look up record %q: %w", name, err)
	}

	// 3. Create or update.
	body, err := buildRecordBody(name, ipv4, proxied)
	if err != nil {
		return err
	}
	method, target := http.MethodPost, dnsRecordCreateURL(zoneID)
	if recordID != "" {
		method, target = http.MethodPut, dnsRecordUpdateURL(zoneID, recordID)
	}
	return cfWrite(ctx, client, apiToken, method, target, body)
}

// cfFirstID does a GET against a Cloudflare list endpoint and returns the id of
// the first result ("" when the list is empty). A non-success payload is turned
// into an error carrying Cloudflare's message.
func cfFirstID(ctx context.Context, client *http.Client, token, target string) (string, error) {
	raw, err := cfDo(ctx, client, token, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	var resp cfListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if !resp.Success {
		return "", cfErr(resp.Errors)
	}
	if len(resp.Result) == 0 {
		return "", nil
	}
	return resp.Result[0].ID, nil
}

// cfWrite does a POST/PUT and verifies the response reports success.
func cfWrite(ctx context.Context, client *http.Client, token, method, target string, body []byte) error {
	raw, err := cfDo(ctx, client, token, method, target, body)
	if err != nil {
		return err
	}
	var resp cfWriteResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !resp.Success {
		return cfErr(resp.Errors)
	}
	return nil
}

// cfDo performs one authenticated JSON request and returns the raw body. A
// non-2xx with an undecodable body is reported with the status line.
func cfDo(ctx context.Context, client *http.Client, token, method, target string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// 2xx → hand the body to the caller to decode. On a non-2xx we still try to
	// surface Cloudflare's JSON error message, falling back to the status.
	if resp.StatusCode/100 != 2 {
		var errResp cfListResponse
		if json.Unmarshal(raw, &errResp) == nil && len(errResp.Errors) > 0 {
			return nil, cfErr(errResp.Errors)
		}
		return nil, fmt.Errorf("cloudflare api: %s", strings.TrimSpace(resp.Status))
	}
	return raw, nil
}

// cfErr collapses Cloudflare's error array into one error.
func cfErr(errs []cfAPIErr) error {
	if len(errs) == 0 {
		return fmt.Errorf("cloudflare api request failed")
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, fmt.Sprintf("%s (%d)", e.Message, e.Code))
	}
	sort.Strings(msgs)
	return fmt.Errorf("cloudflare api: %s", strings.Join(msgs, "; "))
}
