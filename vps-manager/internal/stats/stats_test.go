package stats

import (
	"strings"
	"testing"
)

// hostOutput builds a realistic combined-command response.
const hostOutput = `@@CPU1@@
cpu  10000 200 3000 80000 500 0 100 50 0 0
@@MEM@@
MemTotal:        8047484 kB
MemFree:          512340 kB
MemAvailable:    3936256 kB
@@DISK@@
Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/vda1         82045336 39381761  42663575      48% /
@@LOAD@@
0.52 0.48 0.45 2/345 12345
@@UP@@
1086400.12 2072800.55
@@NPROC@@
4
@@CPU2@@
cpu  10600 200 3200 80800 550 0 110 60 0 0
`

func TestParseHostFull(t *testing.T) {
	h := ParseHost(hostOutput)
	if !h.Available {
		t.Fatal("expected Available")
	}
	// Δtotal = (10600+200+3200+80800+550+0+110+60) - (10000+200+3000+80000+500+0+100+50)
	//        = 95520 - 93850 = 1670
	// ΔidleAll = (80800+550) - (80000+500) = 850 → usage = 1 - 850/1670 ≈ 49.1%
	if h.CPUPercent < 49.0 || h.CPUPercent > 49.2 {
		t.Errorf("CPUPercent = %v, want ≈49.1", h.CPUPercent)
	}
	if h.Cores != 4 {
		t.Errorf("Cores = %d, want 4", h.Cores)
	}
	if h.Load1 != 0.52 {
		t.Errorf("Load1 = %v, want 0.52", h.Load1)
	}
	wantTotal := int64(8047484) * 1024
	if h.MemTotal != wantTotal {
		t.Errorf("MemTotal = %d, want %d", h.MemTotal, wantTotal)
	}
	wantUsed := int64(8047484-3936256) * 1024
	if h.MemUsed != wantUsed {
		t.Errorf("MemUsed = %d, want %d", h.MemUsed, wantUsed)
	}
	if h.MemPercent < 51.0 || h.MemPercent > 51.2 {
		t.Errorf("MemPercent = %v, want ≈51.1", h.MemPercent)
	}
	if h.DiskTotal != 82045336*1024 {
		t.Errorf("DiskTotal = %d", h.DiskTotal)
	}
	if h.DiskUsed != 39381761*1024 {
		t.Errorf("DiskUsed = %d", h.DiskUsed)
	}
	if h.DiskPercent != 48 {
		t.Errorf("DiskPercent = %v, want 48", h.DiskPercent)
	}
	if h.UptimeSecs != 1086400 {
		t.Errorf("UptimeSecs = %d, want 1086400", h.UptimeSecs)
	}
}

func TestParseHostEmpty(t *testing.T) {
	h := ParseHost("")
	if h.Available {
		t.Error("empty output must not be Available")
	}
	if h.CPUPercent != -1 || h.MemPercent != -1 || h.DiskPercent != -1 || h.Load1 != -1 {
		t.Errorf("unknown metrics should be -1: %+v", h)
	}
}

// A host where /proc is missing (Git Bash) but df works: partial data still
// marks the snapshot available.
func TestParseHostPartial(t *testing.T) {
	out := `@@CPU1@@
@@MEM@@
@@DISK@@
Filesystem     1024-blocks     Used Available Capacity Mounted on
C:/Program Files/Git  999935996 578236848 421699148      58% /
@@LOAD@@
@@UP@@
@@NPROC@@
8
@@CPU2@@
`
	h := ParseHost(out)
	if !h.Available {
		t.Fatal("df-only output should still be Available")
	}
	if h.CPUPercent != -1 {
		t.Errorf("CPUPercent = %v, want -1", h.CPUPercent)
	}
	if h.DiskPercent != 58 {
		t.Errorf("DiskPercent = %v, want 58", h.DiskPercent)
	}
	if h.Cores != 8 {
		t.Errorf("Cores = %d, want 8", h.Cores)
	}
}

// Old kernels: no MemAvailable → fall back to MemFree; short cpu line still parses.
func TestParseHostOldKernel(t *testing.T) {
	out := `@@CPU1@@
cpu  100 0 100 800
@@MEM@@
MemTotal:  1000000 kB
MemFree:    400000 kB
@@DISK@@
@@LOAD@@
@@UP@@
@@NPROC@@
@@CPU2@@
cpu  200 0 200 1400
`
	h := ParseHost(out)
	// Δtotal = 1800-1000 = 800, Δidle = 600 → 25%
	if h.CPUPercent != 25 {
		t.Errorf("CPUPercent = %v, want 25", h.CPUPercent)
	}
	wantUsed := int64(600000) * 1024
	if h.MemUsed != wantUsed {
		t.Errorf("MemUsed = %d, want %d (MemFree fallback)", h.MemUsed, wantUsed)
	}
}

func TestParseHostMotdNoise(t *testing.T) {
	h := ParseHost("Welcome to Ubuntu 22.04!\n * Docs: https://help\n" + hostOutput)
	if !h.Available || h.CPUPercent < 0 {
		t.Errorf("leading noise broke parsing: %+v", h)
	}
}

func TestCPUPercentNoDelta(t *testing.T) {
	s := cpuSample{total: 100, idleAll: 50}
	if _, ok := cpuPercent(s, s); ok {
		t.Error("zero delta must not report a percentage")
	}
}

// A rebooted host resets its counters; the idle check must catch the case
// before uint64 subtraction wraps it into a huge positive delta.
func TestCPUPercentCounterReset(t *testing.T) {
	a := cpuSample{total: 1000, idleAll: 800}
	b := cpuSample{total: 1500, idleAll: 700} // idle went "backwards"
	if pct, ok := cpuPercent(a, b); ok {
		t.Errorf("backwards idle counter must not report a percentage, got %v", pct)
	}
}

const containerOutput = `{"ID":"aaa111222333444555666777888999000111222333444555666777888999abc","Image":"ghcr.io/acme/api:5.1.0","Labels":"com.docker.compose.project=checkout,com.docker.compose.service=api,com.docker.compose.version=2.24.5","Names":"checkout-api-1","Ports":"0.0.0.0:8080->8080/tcp","State":"running","Status":"Up 2 days"}
{"ID":"bbb111222333444555666777888999000111222333444555666777888999abc","Image":"postgres:16","Labels":"com.docker.compose.project=checkout,com.docker.compose.service=postgres","Names":"checkout-postgres-1","Ports":"5432/tcp","State":"running","Status":"Up 2 days (healthy)"}
{"ID":"ccc111222333444555666777888999000111222333444555666777888999abc","Image":"redis:7","Labels":"","Names":"standalone-redis","Ports":"","State":"exited","Status":"Exited (0) 3 hours ago"}
@@STATS@@
{"BlockIO":"12.3MB / 4.5MB","CPUPerc":"58.31%","Container":"aaa111222333","ID":"aaa111222333444555666777888999000111222333444555666777888999abc","MemPerc":"47.10%","MemUsage":"481.4MiB / 1GiB","Name":"checkout-api-1","NetIO":"1.45GB / 980MB","PIDs":"23"}
{"BlockIO":"890MB / 3.2GB","CPUPerc":"31.02%","Container":"bbb111222333","ID":"bbb111222333444555666777888999000111222333444555666777888999abc","MemPerc":"62.00%","MemUsage":"1.24GiB / 2GiB","Name":"checkout-postgres-1","NetIO":"12MB / 44MB","PIDs":"9"}
`

func TestParseContainers(t *testing.T) {
	list := ParseContainers(containerOutput)
	if len(list) != 3 {
		t.Fatalf("got %d containers, want 3", len(list))
	}

	api := list[0]
	if api.ID != "aaa111222333" {
		t.Errorf("ID = %q, want short id", api.ID)
	}
	if api.Project != "checkout" || api.Service != "api" {
		t.Errorf("compose labels: project=%q service=%q", api.Project, api.Service)
	}
	if api.CPUPercent != 58.31 {
		t.Errorf("CPUPercent = %v, want 58.31", api.CPUPercent)
	}
	if api.MemPercent != 47.10 {
		t.Errorf("MemPercent = %v", api.MemPercent)
	}
	mib := float64(1 << 20)
	wantUsed := int64(481.4 * mib)
	if api.MemUsed != wantUsed {
		t.Errorf("MemUsed = %d, want %d", api.MemUsed, wantUsed)
	}
	if api.MemLimit != 1<<30 {
		t.Errorf("MemLimit = %d, want %d", api.MemLimit, int64(1<<30))
	}
	if api.PIDs != 23 {
		t.Errorf("PIDs = %d", api.PIDs)
	}

	pg := list[1]
	if pg.Service != "postgres" || pg.CPUPercent != 31.02 {
		t.Errorf("postgres row wrong: %+v", pg)
	}

	redis := list[2]
	if redis.State != "exited" {
		t.Errorf("State = %q", redis.State)
	}
	if redis.CPUPercent != -1 || redis.MemPercent != -1 {
		t.Errorf("stopped container usage should be -1: %+v", redis)
	}
	if redis.Project != "" || redis.Service != "" {
		t.Errorf("standalone container must have no project/service: %+v", redis)
	}
}

func TestParseContainersNoStats(t *testing.T) {
	out := strings.Split(containerOutput, "@@STATS@@")[0]
	list := ParseContainers(out)
	if len(list) != 3 {
		t.Fatalf("got %d containers, want 3", len(list))
	}
	if list[0].CPUPercent != -1 {
		t.Errorf("without stats CPUPercent should be -1")
	}
}

func TestParseContainersEmpty(t *testing.T) {
	if got := ParseContainers("@@STATS@@\n"); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// A label VALUE containing the section marker must not corrupt the split —
// only the echoed marker on its own line divides the ps and stats halves.
func TestParseContainersMarkerInLabel(t *testing.T) {
	out := `{"ID":"aaa111222333","Image":"evil:1","Labels":"note=@@STATS@@,com.docker.compose.project=p1,com.docker.compose.service=svc","Names":"evil-1","Ports":"","State":"running","Status":"Up 1 hour"}
{"ID":"bbb111222333","Image":"nginx:1","Labels":"","Names":"web","Ports":"","State":"running","Status":"Up 2 hours"}
@@STATS@@
{"ID":"aaa111222333","Name":"evil-1","CPUPerc":"1.00%","MemPerc":"2.00%","MemUsage":"10MiB / 1GiB","NetIO":"0B / 0B","BlockIO":"0B / 0B","PIDs":"1"}
`
	list := ParseContainers(out)
	if len(list) != 2 {
		t.Fatalf("got %d containers, want 2 (marker inside a label hid some)", len(list))
	}
	if list[0].Name != "evil-1" || list[1].Name != "web" {
		t.Errorf("wrong containers: %+v", list)
	}
	if list[0].CPUPercent != 1.0 {
		t.Errorf("stats not merged for first container: %v", list[0].CPUPercent)
	}
	if list[0].Project != "p1" || list[0].Service != "svc" {
		t.Errorf("labels mis-parsed: %+v", list[0])
	}
}

func TestLabelValue(t *testing.T) {
	labels := "com.docker.compose.project=my-app,com.docker.compose.service=web,maintainer=x"
	if v := labelValue(labels, "com.docker.compose.project"); v != "my-app" {
		t.Errorf("project = %q", v)
	}
	if v := labelValue(labels, "com.docker.compose.service"); v != "web" {
		t.Errorf("service = %q", v)
	}
	if v := labelValue(labels, "missing"); v != "" {
		t.Errorf("missing = %q", v)
	}
	// A key that is a suffix of another must not match.
	if v := labelValue("xcom.docker.compose.project=evil", "com.docker.compose.project"); v != "" {
		t.Errorf("suffix key matched: %q", v)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0B":       0,
		"481.4MiB": 504784486, // 481.4 * 1048576, truncated
		"1GiB":     1 << 30,
		"2GiB":     2 << 30,
		"1.45GB":   1450000000,
		"980MB":    980000000,
		"736B":     736,
		"1.2kB":    1200,
		"--":       0,
		"":         0,
		"junk":     0,
	}
	for in, want := range cases {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParsePercent(t *testing.T) {
	if v := parsePercent("58.31%"); v != 58.31 {
		t.Errorf("got %v", v)
	}
	if v := parsePercent("--"); v != -1 {
		t.Errorf("got %v", v)
	}
	if v := parsePercent(""); v != -1 {
		t.Errorf("got %v", v)
	}
}
