package maintenance

import (
	"context"
	"testing"
)

func TestParseDF(t *testing.T) {
	out := `Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/sda1         41152736  9123456  29934208      24% /`
	d := parseDF(out)
	if !d.Available {
		t.Fatal("expected df to parse")
	}
	if d.Filesystem != "/dev/sda1" {
		t.Errorf("filesystem = %q", d.Filesystem)
	}
	if d.MountPoint != "/" {
		t.Errorf("mount = %q", d.MountPoint)
	}
	if d.UsePercent != 24 {
		t.Errorf("use%% = %d", d.UsePercent)
	}
	// 41152736 * 1024 = 42140401664
	if d.TotalBytes != 41152736*1024 {
		t.Errorf("total = %d", d.TotalBytes)
	}
	if d.UsedBytes != 9123456*1024 || d.AvailBytes != 29934208*1024 {
		t.Errorf("used/avail = %d/%d", d.UsedBytes, d.AvailBytes)
	}
}

func TestParseDFWrappedDeviceName(t *testing.T) {
	// A long device name wraps onto its own line; the data row still has the
	// six trailing columns.
	out := `Filesystem                              1024-blocks    Used Available Capacity Mounted on
/dev/mapper/very-long-volume-group-name     10485760 2097152   8388608      20% /`
	d := parseDF(out)
	if !d.Available || d.UsePercent != 20 || d.MountPoint != "/" {
		t.Fatalf("bad parse: %+v", d)
	}
}

func TestParseDFGarbage(t *testing.T) {
	if parseDF("").Available {
		t.Error("empty df should be unavailable")
	}
	if parseDF("not a df table at all").Available {
		t.Error("garbage df should be unavailable")
	}
}

func TestParseDockerDFJSON(t *testing.T) {
	// Docker 24/25 NDJSON: one object per line, every value a STRING.
	out := `{"Type":"Images","TotalCount":"12","Active":"4","Size":"3.1GB","Reclaimable":"1.2GB (38%)"}
{"Type":"Containers","TotalCount":"6","Active":"4","Size":"120MB","Reclaimable":"40MB (33%)"}
{"Type":"Local Volumes","TotalCount":"8","Active":"5","Size":"900MB","Reclaimable":"300MB (33%)"}
{"Type":"Build Cache","TotalCount":"30","Active":"0","Size":"500MB","Reclaimable":"500MB"}`
	du, ok := parseDockerDFJSON(out)
	if !ok || !du.Available {
		t.Fatal("expected JSON df to parse")
	}
	if du.Images.TotalCount != 12 || du.Images.ActiveCount != 4 {
		t.Errorf("images counts = %d/%d", du.Images.TotalCount, du.Images.ActiveCount)
	}
	if du.Images.SizeBytes != 3_100_000_000 {
		t.Errorf("images size = %d", du.Images.SizeBytes)
	}
	if du.Images.ReclaimableBytes != 1_200_000_000 {
		t.Errorf("images reclaimable = %d (should strip percentage)", du.Images.ReclaimableBytes)
	}
	if du.Volumes.SizeBytes != 900_000_000 {
		t.Errorf("volumes size = %d", du.Volumes.SizeBytes)
	}
	if du.BuildCache.SizeBytes != 500_000_000 {
		t.Errorf("build cache size = %d", du.BuildCache.SizeBytes)
	}
	// Totals: 3.1G + 120M + 900M + 500M = 4_620_000_000
	if du.TotalBytes != 3_100_000_000+120_000_000+900_000_000+500_000_000 {
		t.Errorf("total = %d", du.TotalBytes)
	}
	// Reclaimable: 1.2G + 40M + 300M + 500M = 2_040_000_000
	if du.ReclaimableBytes != 1_200_000_000+40_000_000+300_000_000+500_000_000 {
		t.Errorf("reclaimable = %d", du.ReclaimableBytes)
	}
}

func TestParseDockerDFJSONUnsupported(t *testing.T) {
	// Old docker echoes the template literally — not JSON. Must report ok=false
	// so the caller falls back to the table form.
	if _, ok := parseDockerDFJSON("{{json .}}"); ok {
		t.Error("non-JSON output should report ok=false")
	}
	if _, ok := parseDockerDFJSON(""); ok {
		t.Error("empty output should report ok=false")
	}
}

func TestParseDockerDFTable(t *testing.T) {
	out := `TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE
Images          12        4         3.1GB     1.2GB (38%)
Containers      6         4         120MB     40MB (33%)
Local Volumes   8         5         900MB     300MB (33%)
Build Cache     30        0         500MB     500MB`
	du, ok := parseDockerDFTable(out)
	if !ok || !du.Available {
		t.Fatal("expected table df to parse")
	}
	if du.Images.SizeBytes != 3_100_000_000 || du.Images.ReclaimableBytes != 1_200_000_000 {
		t.Errorf("images = %+v", du.Images)
	}
	if du.Volumes.TotalCount != 8 || du.Volumes.SizeBytes != 900_000_000 {
		t.Errorf("volumes = %+v (two-word TYPE must parse)", du.Volumes)
	}
	if du.BuildCache.SizeBytes != 500_000_000 {
		t.Errorf("build cache = %+v (no percentage in reclaimable)", du.BuildCache)
	}
}

func TestParseDockerSize(t *testing.T) {
	cases := map[string]int64{
		"0B":    0,
		"0":     0,
		"500MB": 500_000_000,
		"3.1GB": 3_100_000_000,
		"1.2kB": 1_200,
		"2TB":   2_000_000_000_000,
		"100":   100,
		"N/A":   0,
		"":      0,
		"weird": 0,
	}
	for in, want := range cases {
		if got := parseDockerSize(in); got != want {
			t.Errorf("parseDockerSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := map[int64]string{
		0:             "0 B",
		512:           "512 B",
		1500:          "1.5 kB",
		3_100_000_000: "3.1 GB",
	}
	for in, want := range cases {
		if got := humanizeBytes(in); got != want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseImagesSortedLargestFirst(t *testing.T) {
	out := "app\tlatest\tabc123\t120MB\n" +
		"postgres\t16\tdef456\t450MB\n" +
		"redis\t7\t789aaa\t40MB"
	imgs := parseImages(out)
	if len(imgs) != 3 {
		t.Fatalf("n = %d", len(imgs))
	}
	if imgs[0].Repository != "postgres" || imgs[2].Repository != "redis" {
		t.Errorf("not sorted largest-first: %v", []string{imgs[0].Repository, imgs[1].Repository, imgs[2].Repository})
	}
	if imgs[0].SizeBytes != 450_000_000 {
		t.Errorf("postgres size = %d", imgs[0].SizeBytes)
	}
}

func TestPruneCommandFlags(t *testing.T) {
	// Capture the command Prune builds via a fake StreamFn — no network/SSH.
	capture := func(opts PruneOptions) string {
		var cmd string
		stream := func(_ context.Context, c string, _ func(string)) (int, error) {
			cmd = c
			return 0, nil
		}
		if _, err := Prune(context.Background(), stream, opts, nil); err != nil {
			t.Fatalf("Prune err: %v", err)
		}
		return cmd
	}

	// Default: only safe, dangling cleanup — never -a or --volumes.
	if got := capture(PruneOptions{}); got != "docker system prune -f" {
		t.Errorf("default = %q", got)
	}
	// All-images only.
	if got := capture(PruneOptions{AllImages: true}); got != "docker system prune -f -a" {
		t.Errorf("allImages = %q", got)
	}
	// Volumes only — the destructive flag must appear ONLY when requested.
	if got := capture(PruneOptions{Volumes: true}); got != "docker system prune -f --volumes" {
		t.Errorf("volumes = %q", got)
	}
	// Both.
	if got := capture(PruneOptions{AllImages: true, Volumes: true}); got != "docker system prune -f -a --volumes" {
		t.Errorf("both = %q", got)
	}
}
