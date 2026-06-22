package caddy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderOverrideLabelsAndPort(t *testing.T) {
	spec := ExposeSpec{
		StackPath:   "/opt/batlaz",
		MainCompose: "docker-compose.yml",
		Service:     "web",
		Domains:     []string{"app.example.com"},
		Port:        8080,
	}
	out := RenderOverride(spec)

	if !strings.Contains(out, "services:") || !strings.Contains(out, "\n  web:\n") {
		t.Fatalf("override missing service block:\n%s", out)
	}
	if !strings.Contains(out, `caddy: "app.example.com"`) {
		t.Fatalf("override missing caddy domain label:\n%s", out)
	}
	if !strings.Contains(out, `caddy.reverse_proxy: "{{upstreams 8080}}"`) {
		t.Fatalf("override missing/incorrect reverse_proxy upstreams port:\n%s", out)
	}
	// No network attach requested → no networks block.
	if strings.Contains(out, "networks:") {
		t.Fatalf("override should not declare networks when none requested:\n%s", out)
	}
}

func TestRenderOverrideMultiDomain(t *testing.T) {
	spec := ExposeSpec{
		Service: "api",
		Domains: []string{"a.example.com", "b.example.com", "www.example.com"},
		Port:    3000,
	}
	out := RenderOverride(spec)
	if !strings.Contains(out, `caddy: "a.example.com b.example.com www.example.com"`) {
		t.Fatalf("multi-domain not space-joined into one caddy label:\n%s", out)
	}
	if !strings.Contains(out, `caddy.reverse_proxy: "{{upstreams 3000}}"`) {
		t.Fatalf("wrong upstreams port:\n%s", out)
	}
}

func TestRenderOverrideNetworkAttach(t *testing.T) {
	spec := ExposeSpec{
		Service: "web",
		Domains: []string{"x.example.com"},
		Port:    80,
		Network: "caddy",
	}
	out := RenderOverride(spec)
	if !strings.Contains(out, "    networks:\n      - caddy\n") {
		t.Fatalf("service not attached to network:\n%s", out)
	}
	if !strings.Contains(out, "networks:\n  caddy:\n    external: true\n") {
		t.Fatalf("external network not declared:\n%s", out)
	}
}

func TestValidateSpec(t *testing.T) {
	base := ExposeSpec{Service: "web", Domains: []string{"a.example.com"}, Port: 8080}
	if err := validateSpec(base); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	if err := validateSpec(ExposeSpec{Service: "bad name", Domains: []string{"a.com"}, Port: 80}); err == nil {
		t.Fatal("service with space should be rejected")
	}
	if err := validateSpec(ExposeSpec{Service: "web", Domains: []string{"a.com; rm -rf /"}, Port: 80}); err == nil {
		t.Fatal("domain with shell metacharacters should be rejected")
	}
	if err := validateSpec(ExposeSpec{Service: "web", Domains: nil, Port: 80}); err == nil {
		t.Fatal("missing domains should be rejected")
	}
	if err := validateSpec(ExposeSpec{Service: "web", Domains: []string{"a.com"}, Port: 0}); err == nil {
		t.Fatal("port 0 should be rejected")
	}
	if err := validateSpec(ExposeSpec{Service: "web", Domains: []string{"a.com"}, Port: 70000}); err == nil {
		t.Fatal("port > 65535 should be rejected")
	}
}

func TestParseProxyPS(t *testing.T) {
	// Explicit caddy-docker-proxy image wins outright, even with other rows.
	ps := "myapp-web-1\tnginx:latest\n" +
		"infra-caddy-1\tlucaslorentz/caddy-docker-proxy:2.8\n" +
		"myapp-db-1\tpostgres:16"
	info := parseProxyPS(ps)
	if !info.Present || info.Container != "infra-caddy-1" {
		t.Fatalf("expected caddy-docker-proxy detected, got %+v", info)
	}
	if !strings.Contains(info.Image, "caddy-docker-proxy") {
		t.Fatalf("wrong image captured: %q", info.Image)
	}

	// Plain caddy image is taken as a likely proxy fallback.
	info = parseProxyPS("edge\tcaddy:2-alpine\napp\tredis:7")
	if !info.Present || info.Container != "edge" {
		t.Fatalf("plain caddy not detected as fallback: %+v", info)
	}

	// Nothing caddy-ish → not present.
	info = parseProxyPS("app\tnginx:latest\ndb\tpostgres:16")
	if info.Present {
		t.Fatalf("false positive proxy detection: %+v", info)
	}

	// An app whose name merely contains "caddy" must not match.
	info = parseProxyPS("shop\tmycaddyshack/app:1.0")
	if info.Present {
		t.Fatalf("substring 'caddy' in repo should not match: %+v", info)
	}
}

func TestImageLooksLikeCaddy(t *testing.T) {
	cases := map[string]bool{
		"caddy":                               true,
		"caddy:2-alpine":                      true,
		"library/caddy:2":                     true,
		"lucaslorentz/caddy-docker-proxy:2.8": true,
		"docker.io/lucaslorentz/caddy-docker-proxy": true,
		"mycaddyshack/app":                          false,
		"nginx:latest":                              false,
		"postgres:16":                               false,
	}
	for img, want := range cases {
		if got := imageLooksLikeCaddy(img); got != want {
			t.Errorf("imageLooksLikeCaddy(%q) = %v, want %v", img, got, want)
		}
	}
}

func TestRenderCaddyfile(t *testing.T) {
	routes := []Route{
		{Domain: "a.example.com", Upstream: "localhost:8080"},
		{Domain: "b.example.com", Upstream: "127.0.0.1:3000"},
		{Domain: "bad domain", Upstream: "x:1"}, // invalid domain → skipped
		{Domain: "c.example.com", Upstream: ""}, // empty upstream → skipped
	}
	out := RenderCaddyfile(routes)

	if !strings.Contains(out, "a.example.com {\n\treverse_proxy localhost:8080\n}\n") {
		t.Fatalf("first site block wrong:\n%s", out)
	}
	if !strings.Contains(out, "b.example.com {\n\treverse_proxy 127.0.0.1:3000\n}\n") {
		t.Fatalf("second site block wrong:\n%s", out)
	}
	if strings.Contains(out, "bad domain") {
		t.Fatalf("invalid domain leaked into Caddyfile:\n%s", out)
	}
	if strings.Contains(out, "c.example.com") {
		t.Fatalf("route with empty upstream should be skipped:\n%s", out)
	}
}

func TestCloudflareURLBuilders(t *testing.T) {
	if got := zonesURL("example.com"); got != "https://api.cloudflare.com/client/v4/zones?name=example.com" {
		t.Fatalf("zonesURL = %q", got)
	}
	got := dnsRecordsURL("ZONE123", "app.example.com")
	want := "https://api.cloudflare.com/client/v4/zones/ZONE123/dns_records?name=app.example.com&type=A"
	if got != want {
		t.Fatalf("dnsRecordsURL = %q want %q", got, want)
	}
	if got := dnsRecordCreateURL("ZONE123"); got != "https://api.cloudflare.com/client/v4/zones/ZONE123/dns_records" {
		t.Fatalf("dnsRecordCreateURL = %q", got)
	}
	if got := dnsRecordUpdateURL("ZONE123", "REC456"); got != "https://api.cloudflare.com/client/v4/zones/ZONE123/dns_records/REC456" {
		t.Fatalf("dnsRecordUpdateURL = %q", got)
	}
}

func TestBuildRecordBody(t *testing.T) {
	raw, err := buildRecordBody("app.example.com", "203.0.113.7", true)
	if err != nil {
		t.Fatal(err)
	}
	var got dnsRecordBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.Type != "A" {
		t.Errorf("type = %q, want A", got.Type)
	}
	if got.Name != "app.example.com" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Content != "203.0.113.7" {
		t.Errorf("content = %q", got.Content)
	}
	if !got.Proxied {
		t.Error("proxied should be true")
	}
	if got.TTL != 1 {
		t.Errorf("ttl = %d, want 1 (automatic)", got.TTL)
	}

	// proxied=false round-trips too.
	raw, _ = buildRecordBody("a.com", "1.2.3.4", false)
	_ = json.Unmarshal(raw, &got)
	if got.Proxied {
		t.Error("proxied should be false")
	}
}
