package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// execRunner 为默认子进程执行器；Output 在非零退出时仍返回已捕获 stdout，
// smartctl 出错也会输出 JSON，可继续解析。stderr 并入 error 文本
// （供 "no pools available" 之类无结果判定；注入的 fake Runner 照此约定）。
type execRunner struct {
	timeout time.Duration
}

// Run 实现 Runner。
func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s: %w", name, context.DeadlineExceeded)
	}
	if err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			err = fmt.Errorf("%w: %s", err, s)
		}
	}
	return out, err
}

// sysBlockDiskRe 为 /sys/block 下可作 SMART 探测的整盘名（virtio 等无 SMART，排除）。
var sysBlockDiskRe = regexp.MustCompile(`^(sd[a-z]+|hd[a-z]+|nvme\d+n\d+)$`)

var standbyMsgRe = regexp.MustCompile(`(?i)in standby|in sleep|sleep mode|is sleeping`)
var smartUnsupRe = regexp.MustCompile(`(?i)unknown usb bridge|unable to detect device type|lacks smart|smart not supported|please specify device type`)

// smartctlJSON 为 `smartctl -j` 输出的相关子集。
type smartctlJSON struct {
	Smartctl *struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	PowerMode   string `json:"power_mode"`
	SmartStatus *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
	NvmeLog *struct {
		Temperature     *float64 `json:"temperature"` // Kelvin
		CriticalWarning *int64   `json:"critical_warning"`
	} `json:"nvme_smart_health_information_log"`
}

// probeSmart 探测各盘 SMART。绝不唤醒休眠盘：每盘均用 `smartctl -n standby`。
func (p *sysProvider) probeSmart(ctx context.Context) ([]NASDisk, CapStatus) {
	mode := p.cfg.SmartMode
	if mode == "" {
		mode = "auto"
	}
	if mode == "off" {
		return nil, CapUnsupported
	}
	if _, err := p.lookPath("smartctl"); err != nil {
		if mode == "on" {
			return nil, CapFailed // 显式要求但工具缺失
		}
		return nil, CapUnsupported
	}

	devs := p.discoverSmartDevices(ctx)
	if len(devs) == 0 {
		return nil, CapUnsupported
	}

	// 并行探测：-n standby 保证睡盘不被唤醒，并行只缩短超时尾巴。
	results := make([]NASDisk, len(devs))
	var wg sync.WaitGroup
	for i, dev := range devs {
		wg.Add(1)
		go func(i int, dev string) {
			defer wg.Done()
			results[i] = p.smartOne(ctx, dev)
		}(i, dev)
	}
	wg.Wait()

	failed, asleep, unsup := 0, 0, 0
	for _, d := range results {
		switch d.Smart {
		case "failed":
			failed++
		case "asleep":
			asleep++
		case "unsupported":
			unsup++
		}
	}
	switch {
	case failed == len(results):
		return results, CapFailed
	case unsup == len(results):
		return results, CapUnsupported
	case asleep > 0:
		return results, CapAsleep // 契约：有休眠盘未探测 → asleep
	default:
		return results, CapOK
	}
}

// discoverSmartDevices 先走 `smartctl --scan -j`，无结果再回退 /sys/block。
func (p *sysProvider) discoverSmartDevices(ctx context.Context) []string {
	var devs []string
	if out, err := p.runner.Run(ctx, "smartctl", "--scan", "-j"); err == nil {
		var scan struct {
			Devices []struct {
				Name string `json:"name"`
			} `json:"devices"`
		}
		if json.Unmarshal(out, &scan) == nil {
			for _, d := range scan.Devices {
				if d.Name != "" {
					devs = append(devs, d.Name)
				}
			}
		}
	}
	if len(devs) > 0 {
		sort.Strings(devs)
		return devs
	}
	// 回退：/sys/block 整盘。
	if entries, err := p.listDir("/sys/block"); err == nil {
		for _, e := range entries {
			if sysBlockDiskRe.MatchString(e) {
				devs = append(devs, "/dev/"+e)
			}
		}
	}
	sort.Strings(devs)
	return devs
}

// smartOne 探测单盘。dev 形如 /dev/sda；契约上报短名 sda。
func (p *sysProvider) smartOne(ctx context.Context, dev string) NASDisk {
	out, err := p.runner.Run(ctx, "smartctl", "-n", "standby", "-A", "-j", dev)
	state, temp := parseSmartctlA(out)
	if state == "" {
		state = "failed"
		if err == nil && len(out) == 0 {
			state = "unsupported" // 无输出且无错误：异常保守处理
		}
	}
	return NASDisk{Dev: shortDev(dev), Smart: state, TempC: temp}
}

// parseSmartctlA 解析 `smartctl -n standby -A -j` 的 JSON 输出。
// 返回 (state, tempC)：state ∈ ok|failing|asleep|unsupported|failed；空串 = 输出无法解析。
func parseSmartctlA(out []byte) (string, *int) {
	if len(strings.TrimSpace(string(out))) == 0 {
		return "", nil
	}
	var j smartctlJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return "", nil
	}

	// 休眠判定：power_mode 字段优先，messages 兜底（睡盘绝未被唤醒——-n standby 下 smartctl 不触盘）。
	pm := strings.ToUpper(j.PowerMode)
	if strings.Contains(pm, "STANDBY") || strings.Contains(pm, "SLEEP") {
		return "asleep", nil
	}
	standby, unsup := false, false
	if j.Smartctl != nil {
		for _, m := range j.Smartctl.Messages {
			s := m.String
			if smartUnsupRe.MatchString(s) {
				unsup = true
			}
			// 排除 "not in standby" 类反述。
			if standbyMsgRe.MatchString(s) && !strings.Contains(strings.ToLower(s), "not in") {
				standby = true
			}
		}
	}
	if standby {
		return "asleep", nil
	}

	temp := smartTemp(j)

	// 健康判定：smart_status.passed → ok/failing；NVMe critical_warning 兜底。
	if j.SmartStatus != nil && j.SmartStatus.Passed != nil {
		if *j.SmartStatus.Passed {
			return "ok", temp
		}
		return "failing", temp
	}
	if j.NvmeLog != nil && j.NvmeLog.CriticalWarning != nil {
		if *j.NvmeLog.CriticalWarning == 0 {
			return "ok", temp
		}
		return "failing", temp
	}

	// exit_status 位：bit3 (8) = prefail 属性低于阈值 → failing。
	if j.Smartctl != nil && j.Smartctl.ExitStatus&8 != 0 {
		return "failing", temp
	}

	// 无法判定健康：看是否属于「设备不支持」。
	if unsup {
		return "unsupported", temp
	}
	return "failed", temp
}

// smartTemp 取盘温：ATA temperature.current（°C）优先；NVMe composite（Kelvin）换算。
func smartTemp(j smartctlJSON) *int {
	if j.Temperature != nil && j.Temperature.Current != nil {
		v := int(*j.Temperature.Current + 0.5)
		return &v
	}
	if j.NvmeLog != nil && j.NvmeLog.Temperature != nil && *j.NvmeLog.Temperature > 0 {
		v := int(*j.NvmeLog.Temperature - 273.15 + 0.5)
		if v < -50 || v > 200 {
			return nil
		}
		return &v
	}
	return nil
}

// shortDev 把 /dev/sda 规整为 sda（契约 fixture 用短名）。
func shortDev(dev string) string {
	return strings.TrimPrefix(dev, "/dev/")
}
