package collect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// keySets 收集 JSON 各层级的键集合：{"": {top keys}, "sample": {...}, "sample.disks[]": {...}}
func keySets(t *testing.T, data []byte) map[string]map[string]bool {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]map[string]bool{}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			set := out[path]
			if set == nil {
				set = map[string]bool{}
				out[path] = set
			}
			for k, child := range n {
				set[k] = true
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				walk(childPath, child)
			}
		case []any:
			for _, item := range n {
				walk(path+"[]", item)
			}
		}
	}
	walk("", v)
	return out
}

// 与契约 fixture ingest-metrics.json 逐层比对键集合：
// 我们产出的满载载荷必须与契约字段完全一致（防漂移）。
func TestMetricsPayloadMatchesContractFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "ingest-metrics.json"))
	if err != nil {
		t.Skipf("contract fixture not found: %v", err)
	}
	want := keySets(t, fixture)

	exit := 0
	sample := &Sample{
		TS:            NewISOTime(time.Now()),
		CPUPercent:    f64(12.3),
		MemPercent:    f64(45.6),
		MemUsedBytes:  u64(123),
		MemTotalBytes: u64(456),
		Load1:         f64(0.42),
		UptimeSeconds: u64(987654),
		BootTime:      isoPtr(time.Now()),
		Disks: []DiskUsage{
			{Mount: "/", Percent: 55.1, UsedBytes: 1, TotalBytes: 2, Fstype: "ext4"},
		},
		Net: &NetRate{RxBps: 1, TxBps: 2},
		Containers: &[]Container{
			{Name: "feedback", State: "running", ExitCode: nil,
				StartedAt: isoPtr(time.Now()), RestartCount: 0},
			{Name: "kaneo", State: "exited", ExitCode: &exit, RestartCount: 1},
		},
		NAS: &NAS{
			Disks: []NASDisk{{Dev: "sda", Smart: "ok", TempC: intPtr(38)}, {Dev: "sdb", Smart: "asleep"}},
			Pools: []NASPool{{Name: "storage1", State: "ok"}},
		},
	}
	payload := map[string]any{
		"seq":          42,
		"sentAt":       NewISOTime(time.Now()),
		"agent":        map[string]any{"version": "0.1.0", "os": "linux", "arch": "amd64", "hostname": "fn-nas"},
		"capabilities": Capabilities{Containers: CapOK, Smart: CapAsleep, StoragePool: CapOK, Network: CapOK},
		"sample":       sample,
	}
	got := keySets(t, mustJSON(payload))

	for path, keys := range want {
		gset, ok := got[path]
		if !ok && path != "sample.nas.disks[].tempC" {
			t.Fatalf("missing level %q in our payload", path)
		}
		for k := range keys {
			// tempC 在 fixture 中仅出现于非 asleep 盘；我们的满载样本有 ok 盘带 tempC。
			if k == "tempC" && path == "sample.nas.disks[]" {
				continue // 部分条目缺席属契约允许（asleep 无温度）
			}
			if gset != nil && !gset[k] {
				t.Fatalf("missing key %s.%s in our payload", path, k)
			}
		}
	}
	// 反向：我们不得产出契约没有的键（除 nas.disks[].tempC 已豁免）。
	for path, keys := range got {
		wset, ok := want[path]
		if !ok {
			t.Fatalf("unexpected level %q in our payload", path)
		}
		for k := range keys {
			if !wset[k] {
				t.Fatalf("unexpected key %s.%s in our payload", path, k)
			}
		}
	}
}

// 缺席语义：空样本只含 ts；nil 字段一律缺席（绝不输出 0/null 冒充）。
func TestAbsentFieldsOmitted(t *testing.T) {
	s := &Sample{TS: NewISOTime(time.Now())}
	b := mustJSON(s)
	var m map[string]any
	json.Unmarshal(b, &m)
	if len(m) != 1 || m["ts"] == nil {
		t.Fatalf("empty sample should only carry ts: %s", b)
	}
}

// ISOTime 毫秒精度格式。
func TestISOTimeFormat(t *testing.T) {
	b := mustJSON(NewISOTime(time.Date(2026, 9, 18, 6, 29, 58, 123456789, time.UTC)))
	if string(b) != `"2026-09-18T06:29:58.123Z"` {
		t.Fatalf("iso = %s", b)
	}
}

func f64(v float64) *float64 { return &v }
func u64(v uint64) *uint64   { return &v }
func isoPtr(t time.Time) *ISOTime {
	it := NewISOTime(t)
	return &it
}
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
