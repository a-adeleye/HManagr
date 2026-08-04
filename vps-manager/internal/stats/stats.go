// Package stats gathers live host and container metrics for the fleet
// dashboard. Like the rest of the app it shells out and parses ("exec and
// parse"), so it works against any vanilla Linux host over SSH with no agent
// installed on the VPS.
//
// Host metrics come from one combined command (a single SSH round-trip) that
// reads /proc/stat twice with a 1s sleep between the samples — CPU usage is
// the delta between them. Everything is best-effort: a section that fails to
// parse degrades to its zero value (or -1 for "unknown" CPU) instead of
// failing the whole snapshot, since e.g. Git Bash on a local Windows host has
// only a partial /proc.
package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a remote/local command — the same shape the docker, db and
// maintenance packages use, so App.execFn feeds all of them.
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// Host is one live snapshot of a server's vitals.
type Host struct {
	Available   bool    `json:"available"`   // false when nothing could be read
	CPUPercent  float64 `json:"cpuPercent"`  // 0-100 across all cores; -1 = unknown
	Cores       int     `json:"cores"`       // 0 = unknown
	Load1       float64 `json:"load1"`       // 1-minute load average; -1 = unknown
	MemTotal    int64   `json:"memTotal"`    // bytes
	MemUsed     int64   `json:"memUsed"`     // bytes (MemTotal - MemAvailable)
	MemPercent  float64 `json:"memPercent"`  // -1 = unknown
	DiskTotal   int64   `json:"diskTotal"`   // bytes, root filesystem
	DiskUsed    int64   `json:"diskUsed"`    // bytes
	DiskPercent float64 `json:"diskPercent"` // -1 = unknown
	UptimeSecs  int64   `json:"uptimeSecs"`  // 0 = unknown
}

// Container is one container's identity + live usage, tagged with its compose
// project/service so the UI can group services by stack. Usage fields are -1
// (or 0 for the byte counts) when the container isn't running.
type Container struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Image      string  `json:"image"`
	State      string  `json:"state"`  // running / exited / restarting / …
	Status     string  `json:"status"` // human form, e.g. "Up 3 hours"
	Ports      string  `json:"ports"`
	Project    string  `json:"project"` // compose project; "" = standalone
	Service    string  `json:"service"` // compose service; "" = standalone
	CPUPercent float64 `json:"cpuPercent"`
	MemUsed    int64   `json:"memUsed"`  // bytes
	MemLimit   int64   `json:"memLimit"` // bytes
	MemPercent float64 `json:"memPercent"`
	NetIO      string  `json:"netIO"`   // "1.2kB / 800B" as docker reports it
	BlockIO    string  `json:"blockIO"` // ditto
	PIDs       int     `json:"pids"`
}

// Section markers for the combined host command. Each marker sits on its own
// output line so the response splits unambiguously even if a probe fails.
const (
	markCPU1  = "@@CPU1@@"
	markMem   = "@@MEM@@"
	markDisk  = "@@DISK@@"
	markLoad  = "@@LOAD@@"
	markUp    = "@@UP@@"
	markNproc = "@@NPROC@@"
	markCPU2  = "@@CPU2@@"
)

// hostCmd probes everything in one round-trip. Every probe is best-effort
// (`2>/dev/null || true` semantics via `;` chaining), and the 1s sleep between
// the two /proc/stat reads is what defines the CPU sampling window.
const hostCmd = "echo " + markCPU1 + "; grep -m1 '^cpu ' /proc/stat 2>/dev/null" +
	"; echo " + markMem + "; grep -E '^(MemTotal|MemAvailable|MemFree):' /proc/meminfo 2>/dev/null" +
	"; echo " + markDisk + "; df -P -k / 2>/dev/null" +
	"; echo " + markLoad + "; cat /proc/loadavg 2>/dev/null" +
	"; echo " + markUp + "; cat /proc/uptime 2>/dev/null" +
	"; echo " + markNproc + "; nproc 2>/dev/null" +
	"; sleep 1" +
	"; echo " + markCPU2 + "; grep -m1 '^cpu ' /proc/stat 2>/dev/null"

// GetHost takes one live snapshot of the server's vitals. It only errors on
// transport failure (connection gone, context cancelled); unparseable output
// degrades per-field instead.
func GetHost(ctx context.Context, exec ExecFn) (*Host, error) {
	res, err := exec(ctx, hostCmd)
	if err != nil {
		return nil, err
	}
	h := ParseHost(res.Stdout)
	return &h, nil
}

// ParseHost decodes the combined host command's output. Exported for tests.
func ParseHost(out string) Host {
	h := Host{CPUPercent: -1, Load1: -1, MemPercent: -1, DiskPercent: -1}
	sections := splitSections(out)

	cpu1, ok1 := parseCPULine(sections[markCPU1])
	cpu2, ok2 := parseCPULine(sections[markCPU2])
	if ok1 && ok2 {
		if pct, ok := cpuPercent(cpu1, cpu2); ok {
			h.CPUPercent = pct
			h.Available = true
		}
	}

	if total, avail, ok := parseMeminfo(sections[markMem]); ok {
		h.MemTotal = total
		h.MemUsed = total - avail
		if total > 0 {
			h.MemPercent = round1(float64(h.MemUsed) / float64(total) * 100)
		}
		h.Available = true
	}

	if total, used, pct, ok := parseDF(sections[markDisk]); ok {
		h.DiskTotal = total
		h.DiskUsed = used
		h.DiskPercent = pct
		h.Available = true
	}

	if fields := strings.Fields(sections[markLoad]); len(fields) >= 1 {
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			h.Load1 = v
			h.Available = true
		}
	}

	if fields := strings.Fields(sections[markUp]); len(fields) >= 1 {
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			h.UptimeSecs = int64(v)
			h.Available = true
		}
	}

	if n, err := strconv.Atoi(strings.TrimSpace(sections[markNproc])); err == nil && n > 0 {
		h.Cores = n
	}

	return h
}

// splitSections cuts the combined output into marker → body chunks. Unknown
// leading noise (motd fragments, shell warnings) is discarded.
func splitSections(out string) map[string]string {
	sections := map[string]string{}
	current := ""
	var buf []string
	flush := func() {
		if current != "" {
			sections[current] = strings.Join(buf, "\n")
		}
		buf = buf[:0]
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case markCPU1, markMem, markDisk, markLoad, markUp, markNproc, markCPU2:
			flush()
			current = trimmed
		default:
			buf = append(buf, line)
		}
	}
	flush()
	return sections
}

// cpuSample is the aggregate "cpu " line of /proc/stat, in jiffies.
type cpuSample struct {
	total   uint64 // user+nice+system+idle+iowait+irq+softirq+steal
	idleAll uint64 // idle + iowait
}

// parseCPULine reads "cpu  user nice system idle iowait irq softirq steal …".
// Older kernels emit fewer columns; user..idle (4) is the required minimum.
func parseCPULine(s string) (cpuSample, bool) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, false
	}
	var vals []uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, v)
	}
	if len(vals) < 4 {
		return cpuSample{}, false
	}
	// Sum at most the first 8 columns (through steal); guest time is already
	// included in user/nice on Linux, so adding it would double-count.
	n := len(vals)
	if n > 8 {
		n = 8
	}
	var sample cpuSample
	for i := 0; i < n; i++ {
		sample.total += vals[i]
	}
	sample.idleAll = vals[3]
	if len(vals) > 4 {
		sample.idleAll += vals[4]
	}
	return sample, true
}

// cpuPercent is usage across the sampling window: 1 - Δidle/Δtotal. Counters
// that went backwards (a reboot between samples) make the window meaningless,
// so report not-ok rather than a garbage percentage. The idle check must
// happen before the uint64 subtraction — unsigned underflow can't go negative.
func cpuPercent(a, b cpuSample) (float64, bool) {
	if b.total <= a.total || b.idleAll < a.idleAll {
		return 0, false
	}
	dTotal := float64(b.total - a.total)
	dIdle := float64(b.idleAll - a.idleAll)
	pct := (1 - dIdle/dTotal) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return round1(pct), true
}

// parseMeminfo reads MemTotal/MemAvailable (falling back to MemFree on old
// kernels without MemAvailable). Values are "NNN kB".
func parseMeminfo(s string) (total, avail int64, ok bool) {
	var free int64
	haveAvail := false
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		v *= 1024 // meminfo is in kB
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = v
		case "MemAvailable":
			avail = v
			haveAvail = true
		case "MemFree":
			free = v
		}
	}
	if total <= 0 {
		return 0, 0, false
	}
	if !haveAvail {
		avail = free
	}
	return total, avail, true
}

// parseDF reads `df -P -k /`: a header line then one data row whose last two
// columns are "NN%" and the mount point, with sizes in 1K blocks. A long
// device name can wrap the row onto two lines, so scan for the row shape
// rather than assuming line two.
func parseDF(s string) (total, used int64, pct float64, ok bool) {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] == "Filesystem" {
			continue
		}
		capStr := fields[len(fields)-2]
		if !strings.HasSuffix(capStr, "%") {
			continue
		}
		t, err1 := strconv.ParseInt(fields[len(fields)-5], 10, 64)
		u, err2 := strconv.ParseInt(fields[len(fields)-4], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		p, _ := strconv.ParseFloat(strings.TrimSuffix(capStr, "%"), 64)
		return t * 1024, u * 1024, p, true
	}
	return 0, 0, 0, false
}

// ───────── Containers ─────────

const markStats = "@@STATS@@"

// GetContainers lists every container with identity from `docker ps -a` and
// live usage from `docker stats --no-stream`, in one round-trip. Stopped
// containers appear with usage -1/0 (docker stats only reports running ones).
func GetContainers(ctx context.Context, exec ExecFn) ([]Container, error) {
	// `rc=$?` / `exit $rc` preserve docker ps's exit code past the stats half:
	// ps is the authoritative list, and stats failing (a daemon race, an old
	// docker) should degrade to "no live usage", not an error.
	cmd := `docker ps -a --no-trunc --format '{{json .}}'; rc=$?` +
		`; echo ` + markStats +
		`; docker stats --no-stream --no-trunc --format '{{json .}}' 2>/dev/null` +
		`; exit $rc`
	res, err := exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 && !strings.Contains(res.Stdout, "{") {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return nil, fmt.Errorf("docker ps: %s", msg)
	}
	return ParseContainers(res.Stdout), nil
}

// psRow is one line of `docker ps -a --format '{{json .}}'`.
type psRow struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
	Labels string `json:"Labels"`
}

// statsRow is one line of `docker stats --no-stream --format '{{json .}}'`.
// Every value is a formatted string ("1.23%", "24.5MiB / 1.9GiB").
type statsRow struct {
	ID       string `json:"ID"`
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
	PIDs     string `json:"PIDs"`
}

// ParseContainers merges the ps and stats halves of GetContainers' output.
// Exported for tests.
func ParseContainers(out string) []Container {
	// Split on the marker LINE, not the first occurrence of the marker string:
	// label values inside the ps JSON are arbitrary (a compose file can set
	// any label), so a value containing "@@STATS@@" must not corrupt the
	// split. The echoed delimiter is always alone on its line; JSON data
	// lines start with '{' and can never trim-equal it.
	var psLines, statsLines []string
	inStats := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == markStats {
			inStats = true
			continue
		}
		if inStats {
			statsLines = append(statsLines, line)
		} else {
			psLines = append(psLines, line)
		}
	}
	psPart := strings.Join(psLines, "\n")
	statsPart := strings.Join(statsLines, "\n")

	// Index live usage by full and short ID plus name — `docker ps` shows a
	// 12-char ID unless --no-trunc, and matching by prefix covers both.
	usage := map[string]statsRow{}
	for _, line := range strings.Split(statsPart, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var row statsRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.ID != "" {
			usage[row.ID] = row
			if len(row.ID) > 12 {
				usage[row.ID[:12]] = row
			}
		}
		if row.Name != "" {
			usage["name:"+row.Name] = row
		}
	}

	var containers []Container
	for _, line := range strings.Split(psPart, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var row psRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		c := Container{
			ID:         row.ID,
			Name:       row.Names,
			Image:      row.Image,
			State:      row.State,
			Status:     row.Status,
			Ports:      row.Ports,
			CPUPercent: -1,
			MemPercent: -1,
		}
		if len(c.ID) > 12 {
			c.ID = c.ID[:12]
		}
		c.Project = labelValue(row.Labels, "com.docker.compose.project")
		c.Service = labelValue(row.Labels, "com.docker.compose.service")

		u, ok := usage[row.ID]
		if !ok {
			u, ok = usage[c.ID]
		}
		if !ok {
			u, ok = usage["name:"+row.Names]
		}
		if ok {
			c.CPUPercent = parsePercent(u.CPUPerc)
			c.MemPercent = parsePercent(u.MemPerc)
			c.MemUsed, c.MemLimit = parseMemUsage(u.MemUsage)
			c.NetIO = u.NetIO
			c.BlockIO = u.BlockIO
			c.PIDs, _ = strconv.Atoi(strings.TrimSpace(u.PIDs))
		}
		containers = append(containers, c)
	}
	return containers
}

// labelValue extracts one label from docker ps's comma-joined "k=v,k=v" list.
// Label values can themselves contain commas (e.g. compose's config-files on
// some setups), so match on the "key=" prefix per segment rather than
// splitting values further.
func labelValue(labels, key string) string {
	for _, part := range strings.Split(labels, ",") {
		if strings.HasPrefix(part, key+"=") {
			return part[len(key)+1:]
		}
	}
	return ""
}

// parsePercent reads docker's "1.23%" form; -1 when absent/unparseable.
func parsePercent(s string) float64 {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	if s == "" || s == "--" {
		return -1
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v
}

// parseMemUsage splits docker's "24.5MiB / 1.9GiB" into used/limit bytes.
func parseMemUsage(s string) (used, limit int64) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) >= 1 {
		used = parseSize(parts[0])
	}
	if len(parts) == 2 {
		limit = parseSize(parts[1])
	}
	return used, limit
}

// parseSize converts a docker human size to bytes. docker stats uses binary
// units (KiB/MiB/GiB); docker ps/system df use decimal (kB/MB/GB) — accept
// both. Unparseable input yields 0.
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "--" || s == "N/A" {
		return 0
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	var mult float64
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "", "B":
		mult = 1
	case "KB", "K":
		mult = 1e3
	case "MB", "M":
		mult = 1e6
	case "GB", "G":
		mult = 1e9
	case "TB", "T":
		mult = 1e12
	case "KIB":
		mult = 1 << 10
	case "MIB":
		mult = 1 << 20
	case "GIB":
		mult = 1 << 30
	case "TIB":
		mult = 1 << 40
	default:
		return 0
	}
	return int64(num * mult)
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
