// Package maintenance backs a "system maintenance" panel for a VPS: it reports
// how much disk the root filesystem and Docker are using, and reclaims Docker's
// dangling space on demand. Like the rest of the app it works by shelling out
// over SSH and parsing the output ("exec and parse") rather than talking to any
// API, so it runs against any vanilla host with df + docker.
//
// Two read commands feed Usage:
//
//   - `df -P /`        — POSIX-portable, fixed columns, sizes in 1K blocks.
//   - `docker system df --format json` — Docker's machine-readable disk report.
//
// The `docker system df --format '{{json .}}'` shape we target is the Docker
// 24/25 one: ONE JSON object PER LINE (NDJSON), one line per resource type, e.g.
//
//	{"Type":"Images","TotalCount":"12","Active":"4","Size":"3.1GB","Reclaimable":"1.2GB (38%)"}
//	{"Type":"Containers","TotalCount":"6","Active":"4","Size":"120MB","Reclaimable":"40MB (33%)"}
//	{"Type":"Local Volumes","TotalCount":"8","Active":"5","Size":"900MB","Reclaimable":"300MB (33%)"}
//	{"Type":"Build Cache","TotalCount":"30","Active":"0","Size":"500MB","Reclaimable":"500MB"}
//
// Sizes there are human strings ("3.1GB", "500MB"), so we parse them back to
// bytes. If `--format json` isn't supported (older Docker) we fall back to the
// line-table form of plain `docker system df`. Either way the report is
// best-effort: a missing or unparseable field degrades to zero rather than
// failing the whole panel.
package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a buffered remote command (possibly sudo-wrapped by the caller so
// docker and df can read root-only paths).
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a remote command and streams combined output as it arrives.
// Returns the exit code.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// DiskUsage is the root filesystem's usage, parsed from `df -P /`.
type DiskUsage struct {
	Filesystem string `json:"filesystem"`
	TotalBytes int64  `json:"totalBytes"`
	UsedBytes  int64  `json:"usedBytes"`
	AvailBytes int64  `json:"availBytes"`
	UsePercent int    `json:"usePercent"`
	MountPoint string `json:"mountPoint"`
	TotalHuman string `json:"totalHuman"`
	UsedHuman  string `json:"usedHuman"`
	AvailHuman string `json:"availHuman"`
	Available  bool   `json:"available"` // false when df couldn't be read/parsed
}

// DockerCategory is one row of `docker system df` (Images, Containers, Local
// Volumes, Build Cache). Reclaimable is the space `docker system prune` could
// free for that category.
type DockerCategory struct {
	Type             string `json:"type"`
	TotalCount       int    `json:"totalCount"`
	ActiveCount      int    `json:"activeCount"`
	SizeBytes        int64  `json:"sizeBytes"`
	ReclaimableBytes int64  `json:"reclaimableBytes"`
	SizeHuman        string `json:"sizeHuman"`
	ReclaimableHuman string `json:"reclaimableHuman"`
}

// DockerUsage is the parsed `docker system df`, plus convenience totals.
type DockerUsage struct {
	Images           DockerCategory `json:"images"`
	Containers       DockerCategory `json:"containers"`
	Volumes          DockerCategory `json:"volumes"`
	BuildCache       DockerCategory `json:"buildCache"`
	TotalBytes       int64          `json:"totalBytes"`
	ReclaimableBytes int64          `json:"reclaimableBytes"`
	TotalHuman       string         `json:"totalHuman"`
	ReclaimableHuman string         `json:"reclaimableHuman"`
	Available        bool           `json:"available"` // false when docker df couldn't be read
}

// Usage is the whole panel snapshot.
type Usage struct {
	Disk   DiskUsage   `json:"disk"`
	Docker DockerUsage `json:"docker"`
}

// Usage gathers root-fs and Docker disk usage in one shot. Each half is
// best-effort: if one command fails the other still populates, and the failing
// half is returned with Available=false rather than erroring. A hard transport
// error (context cancelled, connection dropped) is still returned.
func GetUsage(ctx context.Context, exec ExecFn) (*Usage, error) {
	u := &Usage{}

	// Root filesystem: df -P / gives portable, fixed columns in 1K blocks.
	res, err := exec(ctx, "df -P /")
	if err != nil {
		return nil, err
	}
	if res != nil && res.ExitCode == 0 {
		u.Disk = parseDF(res.Stdout)
	}

	// Docker disk: prefer the machine-readable JSON form; fall back to the
	// line-table when --format json isn't supported (older Docker).
	res, err = exec(ctx, "docker system df --format '{{json .}}'")
	if err != nil {
		return nil, err
	}
	if res != nil && res.ExitCode == 0 {
		if du, ok := parseDockerDFJSON(res.Stdout); ok {
			u.Docker = du
		}
	}
	if !u.Docker.Available {
		// JSON unsupported or unparseable → try the human table.
		if res, err := exec(ctx, "docker system df"); err == nil && res != nil && res.ExitCode == 0 {
			if du, ok := parseDockerDFTable(res.Stdout); ok {
				u.Docker = du
			}
		}
	}
	return u, nil
}

// PruneOptions controls how aggressive Prune is. The defaults (both false) only
// remove dangling images, stopped containers, unused networks and dangling build
// cache — nothing that holds user data. The two destructive switches are opt-in.
type PruneOptions struct {
	// AllImages adds `-a`: also remove ALL images not used by a container (not
	// just dangling ones). They'd have to be re-pulled/rebuilt.
	AllImages bool `json:"allImages"`
	// Volumes adds `--volumes`: also remove anonymous unused volumes. This
	// DELETES DATA and is never implied — it must be explicitly requested.
	Volumes bool `json:"volumes"`
}

// Prune streams `docker system prune -f`, adding `-a` and/or `--volumes` only
// when opts ask for them. It NEVER passes those flags otherwise, since they can
// destroy images and volume data. Returns the remote exit code.
func Prune(ctx context.Context, stream StreamFn, opts PruneOptions, onOutput func(string)) (int, error) {
	cmd := "docker system prune -f"
	if opts.AllImages {
		cmd += " -a"
	}
	if opts.Volumes {
		cmd += " --volumes"
	}
	return stream(ctx, cmd, onOutput)
}

// Image is one row of ListLargestImages.
type Image struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	ID         string `json:"id"`
	SizeBytes  int64  `json:"sizeBytes"`
	SizeHuman  string `json:"sizeHuman"`
}

// ListLargestImages returns up to limit images, biggest first — a cheap way for
// the panel to show what's eating space. It reads `docker images` in a stable
// tab-separated format and sorts client-side, so it doesn't depend on any
// particular docker sort support. Best-effort: a failure yields an empty slice.
func ListLargestImages(ctx context.Context, exec ExecFn, limit int) ([]Image, error) {
	format := "{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.Size}}"
	res, err := exec(ctx, "docker images --format "+shellQuote(format))
	if err != nil {
		return nil, err
	}
	if res == nil || res.ExitCode != 0 {
		return nil, nil
	}
	imgs := parseImages(res.Stdout)
	if limit > 0 && len(imgs) > limit {
		imgs = imgs[:limit]
	}
	return imgs, nil
}

// ───────── parsing (pure, unit-tested) ─────────

// parseDF reads `df -P /` output. The -P (POSIX) format is a header line then
// one data line with six fixed columns:
//
//	Filesystem 1024-blocks Used Available Capacity Mounted on
//	/dev/sda1   41152736  9123456  29934208      24% /
//
// Sizes are in 1024-byte blocks; we convert to bytes. A wrapped long device name
// can push the row onto two physical lines, so we scan from the end for the row
// that has the trailing percentage + mount point.
func parseDF(out string) DiskUsage {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		// Header line starts with "Filesystem"; skip it.
		if fields[0] == "Filesystem" {
			continue
		}
		// Capacity column is the second-to-last "Mounted on" pair: the last
		// field is the mount point, the one before it the NN% capacity.
		mount := fields[len(fields)-1]
		capStr := fields[len(fields)-2]
		if !strings.HasSuffix(capStr, "%") {
			continue
		}
		total, ok1 := parseBlocks(fields[len(fields)-5])
		used, ok2 := parseBlocks(fields[len(fields)-4])
		avail, ok3 := parseBlocks(fields[len(fields)-3])
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		pct, _ := strconv.Atoi(strings.TrimSuffix(capStr, "%"))
		return DiskUsage{
			Filesystem: fields[0],
			TotalBytes: total,
			UsedBytes:  used,
			AvailBytes: avail,
			UsePercent: pct,
			MountPoint: mount,
			TotalHuman: humanizeBytes(total),
			UsedHuman:  humanizeBytes(used),
			AvailHuman: humanizeBytes(avail),
			Available:  true,
		}
	}
	return DiskUsage{}
}

// parseBlocks converts a df 1024-block count to bytes.
func parseBlocks(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n * 1024, true
}

// dockerDFRow is one NDJSON line of `docker system df --format '{{json .}}'`.
// Every value is emitted as a STRING by Docker's template, including the counts.
type dockerDFRow struct {
	Type        string `json:"Type"`
	TotalCount  string `json:"TotalCount"`
	Active      string `json:"Active"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
}

// parseDockerDFJSON decodes the NDJSON form (one object per line). It returns
// ok=false when nothing decoded, so the caller can fall back to the table form.
func parseDockerDFJSON(out string) (DockerUsage, bool) {
	du := DockerUsage{}
	any := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var row dockerDFRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		cat := rowToCategory(row)
		if assignCategory(&du, cat) {
			any = true
		}
	}
	if !any {
		return DockerUsage{}, false
	}
	finalizeTotals(&du)
	return du, true
}

// rowToCategory turns a decoded JSON/table row into a DockerCategory, parsing
// the human sizes back to bytes. Reclaimable is "1.2GB (38%)" — we keep only the
// size before the parenthetical percentage.
func rowToCategory(row dockerDFRow) DockerCategory {
	reclaim := strings.TrimSpace(row.Reclaimable)
	if i := strings.IndexByte(reclaim, '('); i >= 0 {
		reclaim = strings.TrimSpace(reclaim[:i])
	}
	total, _ := strconv.Atoi(strings.TrimSpace(row.TotalCount))
	active, _ := strconv.Atoi(strings.TrimSpace(row.Active))
	sizeB := parseDockerSize(row.Size)
	reclaimB := parseDockerSize(reclaim)
	return DockerCategory{
		Type:             strings.TrimSpace(row.Type),
		TotalCount:       total,
		ActiveCount:      active,
		SizeBytes:        sizeB,
		ReclaimableBytes: reclaimB,
		SizeHuman:        humanizeBytes(sizeB),
		ReclaimableHuman: humanizeBytes(reclaimB),
	}
}

// assignCategory routes a category into the right field by its Type label.
// Docker labels volumes "Local Volumes" and cache "Build Cache". Returns false
// for an unrecognized Type so it doesn't count as a successful parse on its own.
func assignCategory(du *DockerUsage, cat DockerCategory) bool {
	switch normalizeType(cat.Type) {
	case "images":
		du.Images = cat
	case "containers":
		du.Containers = cat
	case "localvolumes", "volumes":
		du.Volumes = cat
	case "buildcache":
		du.BuildCache = cat
	default:
		return false
	}
	return true
}

func normalizeType(t string) string {
	t = strings.ToLower(t)
	t = strings.ReplaceAll(t, " ", "")
	return t
}

// finalizeTotals sums the per-category bytes and fills the human strings, and
// marks the report available.
func finalizeTotals(du *DockerUsage) {
	du.TotalBytes = du.Images.SizeBytes + du.Containers.SizeBytes + du.Volumes.SizeBytes + du.BuildCache.SizeBytes
	du.ReclaimableBytes = du.Images.ReclaimableBytes + du.Containers.ReclaimableBytes + du.Volumes.ReclaimableBytes + du.BuildCache.ReclaimableBytes
	du.TotalHuman = humanizeBytes(du.TotalBytes)
	du.ReclaimableHuman = humanizeBytes(du.ReclaimableBytes)
	du.Available = true
}

// parseDockerDFTable parses the plain `docker system df` table (the fallback
// when --format json isn't supported):
//
//	TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE
//	Images          12        4         3.1GB     1.2GB (38%)
//	Containers      6         4         120MB     40MB (33%)
//	Local Volumes   8         5         900MB     300MB (33%)
//	Build Cache     30        0         500MB     500MB
//
// "Local Volumes" and "Build Cache" are two-word TYPE labels, so we parse from
// the RIGHT (RECLAIMABLE may itself be "1.2GB (38%)" = two tokens) and treat the
// remaining leading tokens as the TYPE.
func parseDockerDFTable(out string) (DockerUsage, bool) {
	du := DockerUsage{}
	any := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "TYPE") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// Reclaimable may be "<size> (NN%)" → two trailing tokens, else one.
		idx := len(fields)
		var reclaim string
		if strings.HasPrefix(fields[idx-1], "(") {
			reclaim = fields[idx-2]
			idx -= 2
		} else {
			reclaim = fields[idx-1]
			idx--
		}
		size := fields[idx-1]
		active := fields[idx-2]
		total := fields[idx-3]
		typ := strings.Join(fields[:idx-3], " ")
		row := dockerDFRow{Type: typ, TotalCount: total, Active: active, Size: size, Reclaimable: reclaim}
		if assignCategory(&du, rowToCategory(row)) {
			any = true
		}
	}
	if !any {
		return DockerUsage{}, false
	}
	finalizeTotals(&du)
	return du, true
}

// parseImages reads the tab-separated `docker images` output into a list sorted
// largest-first.
func parseImages(out string) []Image {
	var imgs []Image
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		size := parseDockerSize(parts[3])
		imgs = append(imgs, Image{
			Repository: strings.TrimSpace(parts[0]),
			Tag:        strings.TrimSpace(parts[1]),
			ID:         strings.TrimSpace(parts[2]),
			SizeBytes:  size,
			SizeHuman:  humanizeBytes(size),
		})
	}
	// Sort largest first (simple insertion-free selection on a small slice).
	for i := 0; i < len(imgs); i++ {
		max := i
		for j := i + 1; j < len(imgs); j++ {
			if imgs[j].SizeBytes > imgs[max].SizeBytes {
				max = j
			}
		}
		imgs[i], imgs[max] = imgs[max], imgs[i]
	}
	return imgs
}

// parseDockerSize converts a docker human size ("3.1GB", "500MB", "1.2kB",
// "0B") to bytes. Docker uses decimal SI units (kB=1000) in `system df`. An
// empty or unparseable value yields 0 so the report degrades gracefully.
func parseDockerSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	// Split the numeric prefix from the unit suffix.
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
	unit := strings.ToUpper(strings.TrimSpace(s[i:]))
	var mult float64 = 1
	switch unit {
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
	case "PB", "P":
		mult = 1e15
	default:
		return 0
	}
	return int64(num * mult)
}

// humanizeBytes renders a byte count as a compact decimal-SI string ("3.1 GB"),
// matching how Docker presents sizes. 0 renders as "0 B".
func humanizeBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"kB", "MB", "GB", "TB", "PB"}
	val := float64(n)
	for _, u := range units {
		val /= 1000
		if val < 1000 {
			return fmt.Sprintf("%.1f %s", val, u)
		}
	}
	return fmt.Sprintf("%.1f PB", val)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
