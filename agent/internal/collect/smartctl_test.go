package collect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeRunner 按命令名分发预置输出。
type fakeRunner struct {
	outputs map[string]fakeRunResult
	calls   []string
}

type fakeRunResult struct {
	out []byte
	err error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if r, ok := f.outputs[key]; ok {
		return r.out, r.err
	}
	// 前缀兜底：smartctl -n standby -A -j /dev/X
	for k, r := range f.outputs {
		if strings.HasPrefix(key, k) {
			return r.out, r.err
		}
	}
	return nil, errors.New("unexpected command: " + key)
}

const smartAwakeOK = `{
  "json_format_version": [1, 0],
  "smartctl": {"version": [7, 4], "exit_status": 0},
  "device": {"name": "/dev/sda", "type": "scsi"},
  "smart_status": {"passed": true},
  "temperature": {"current": 38},
  "power_mode": "ACTIVE"
}`

const smartStandby = `{
  "json_format_version": [1, 0],
  "smartctl": {
    "version": [7, 4],
    "exit_status": 0,
    "messages": [{"string": "Device is in STANDBY mode, exit(2)"}]
  },
  "device": {"name": "/dev/sdb", "type": "scsi"},
  "power_mode": "STANDBY"
}`

const smartFailing = `{
  "smartctl": {"exit_status": 8},
  "smart_status": {"passed": false},
  "temperature": {"current": 51}
}`

const smartNvme = `{
  "smartctl": {"exit_status": 0},
  "smart_status": {"passed": true},
  "nvme_smart_health_information_log": {
    "critical_warning": 0,
    "temperature": 313
  }
}`

const smartUnsupDevice = `{
  "smartctl": {
    "exit_status": 1,
    "messages": [{"string": "Unknown USB bridge [0x1234], please specify device type"}]
  }
}`

func TestParseSmartctlA(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		wantState string
		wantTemp  *int
	}{
		{"awake ok+temp", smartAwakeOK, "ok", intPtr(38)},
		{"standby → asleep 无温度", smartStandby, "asleep", nil},
		{"failing", smartFailing, "failing", intPtr(51)},
		{"nvme Kelvin→°C", smartNvme, "ok", intPtr(40)},
		{"unsupported device", smartUnsupDevice, "unsupported", nil},
		{"空输出", "", "", nil},
		{"非 JSON", "smartctl 6.5 blah", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, temp := parseSmartctlA([]byte(c.out))
			if state != c.wantState {
				t.Fatalf("state = %q; want %q", state, c.wantState)
			}
			if (temp == nil) != (c.wantTemp == nil) {
				t.Fatalf("temp = %v; want %v", temp, c.wantTemp)
			}
			if temp != nil && *temp != *c.wantTemp {
				t.Fatalf("temp = %d; want %d", *temp, *c.wantTemp)
			}
		})
	}
}

// 关键不变式：睡盘绝不返回温度，且探测调用必须带 -n standby（不唤醒）。
func TestSmartProbeNeverWakesDisks(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]fakeRunResult{
		"smartctl --scan -j":                 {out: []byte(`{"devices":[{"name":"/dev/sda"},{"name":"/dev/sdb"}]}`)},
		"smartctl -n standby -A -j /dev/sda": {out: []byte(smartAwakeOK)},
		"smartctl -n standby -A -j /dev/sdb": {out: []byte(smartStandby)},
	}}
	p := newSysProvider(ProviderConfig{
		SmartMode:  "on",
		Runner:     runner,
		LookPath:   func(name string) (string, error) { return "/usr/sbin/" + name, nil },
		DockerHost: "",
	}, true)

	disks, cap := p.probeSmart(context.Background())
	if cap != CapAsleep {
		t.Fatalf("cap = %s; want asleep（有休眠盘）", cap)
	}
	if len(disks) != 2 {
		t.Fatalf("disks = %+v", disks)
	}
	var sda, sdb *NASDisk
	for i := range disks {
		switch disks[i].Dev {
		case "sda":
			sda = &disks[i]
		case "sdb":
			sdb = &disks[i]
		}
	}
	if sda == nil || sda.Smart != "ok" || sda.TempC == nil || *sda.TempC != 38 {
		t.Fatalf("sda = %+v", sda)
	}
	if sdb == nil || sdb.Smart != "asleep" || sdb.TempC != nil {
		t.Fatalf("sdb = %+v", sdb)
	}
	// 所有触盘调用必须带 -n standby。
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "smartctl") && strings.Contains(call, "/dev/") {
			if !strings.Contains(call, "-n standby") {
				t.Fatalf("call without -n standby: %s（可能唤醒睡盘）", call)
			}
		}
	}
}

// 能力聚合：全 ok→ok；无 smartctl→unsupported(auto)/failed(on)；全部失败→failed。
func TestSmartCapabilityMatrix(t *testing.T) {
	// off
	p := newSysProvider(ProviderConfig{SmartMode: "off"}, true)
	if _, cap := p.probeSmart(context.Background()); cap != CapUnsupported {
		t.Fatalf("off → %s; want unsupported", cap)
	}
	// auto + 无 smartctl 二进制
	p = newSysProvider(ProviderConfig{
		SmartMode: "auto",
		LookPath:  func(string) (string, error) { return "", errors.New("not found") },
	}, true)
	if _, cap := p.probeSmart(context.Background()); cap != CapUnsupported {
		t.Fatalf("auto+missing → %s; want unsupported", cap)
	}
	// on + 无 smartctl 二进制
	p = newSysProvider(ProviderConfig{
		SmartMode: "on",
		LookPath:  func(string) (string, error) { return "", errors.New("not found") },
	}, true)
	if _, cap := p.probeSmart(context.Background()); cap != CapFailed {
		t.Fatalf("on+missing → %s; want failed", cap)
	}
	// 有 smartctl 但无盘（VPS virtio）
	runner := &fakeRunner{outputs: map[string]fakeRunResult{
		"smartctl --scan -j": {out: []byte(`{"devices":[]}`)},
	}}
	p = newSysProvider(ProviderConfig{
		SmartMode: "auto",
		Runner:    runner,
		LookPath:  func(n string) (string, error) { return "/usr/sbin/" + n, nil },
		ListDir:   func(string) ([]string, error) { return []string{"vda", "lo"}, nil },
	}, true)
	if _, cap := p.probeSmart(context.Background()); cap != CapUnsupported {
		t.Fatalf("no disks → %s; want unsupported", cap)
	}
	// 全部盘探测失败 → failed
	runner2 := &fakeRunner{outputs: map[string]fakeRunResult{
		"smartctl --scan -j":                 {out: []byte(`{"devices":[{"name":"/dev/sda"}]}`)},
		"smartctl -n standby -A -j /dev/sda": {out: nil, err: errors.New("exec error")},
	}}
	p = newSysProvider(ProviderConfig{
		SmartMode: "on",
		Runner:    runner2,
		LookPath:  func(n string) (string, error) { return "/usr/sbin/" + n, nil },
	}, true)
	if _, cap := p.probeSmart(context.Background()); cap != CapFailed {
		t.Fatalf("all failed → %s; want failed", cap)
	}
}

// /sys/block 回退发现。
func TestSmartDiscoverSysBlockFallback(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]fakeRunResult{
		"smartctl --scan -j":                     {out: nil, err: errors.New("scan failed")},
		"smartctl -n standby -A -j /dev/sda":     {out: []byte(smartAwakeOK)},
		"smartctl -n standby -A -j /dev/nvme0n1": {out: []byte(smartNvme)},
	}}
	p := newSysProvider(ProviderConfig{
		SmartMode: "auto",
		Runner:    runner,
		LookPath:  func(n string) (string, error) { return "/usr/sbin/" + n, nil },
		ListDir: func(dir string) ([]string, error) {
			if dir != "/sys/block" {
				t.Fatalf("listDir %s", dir)
			}
			return []string{"sda", "nvme0n1", "vda", "dm-0", "loop0"}, nil
		},
	}, true)
	disks, cap := p.probeSmart(context.Background())
	if cap != CapOK {
		t.Fatalf("cap = %s; want ok", cap)
	}
	if len(disks) != 2 {
		t.Fatalf("disks = %+v; want sda+nvme0n1（vda/dm/loop 排除）", disks)
	}
}

func intPtr(v int) *int { return &v }

// 确保 NASDisk JSON 序列化与契约一致（asleep 无 tempC 键）。
func TestNASDiskJSONShape(t *testing.T) {
	d := NASDisk{Dev: "sdb", Smart: "asleep"}
	b, _ := json.Marshal(d)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, has := m["tempC"]; has {
		t.Fatalf("asleep disk must not carry tempC: %s", b)
	}
	if m["dev"] != "sdb" || m["smart"] != "asleep" {
		t.Fatalf("bad json: %s", b)
	}
}
