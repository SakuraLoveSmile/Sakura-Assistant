package collect

import (
	"context"
	"regexp"
	"strings"
)

// 存储池探测：btrfs / zpool / mdadm(仅 Linux，/proc/mdstat)。
// 三者互补：fnOS 常见 btrfs；VPS 常见全无 → unsupported。

var (
	btrfsLabelRe   = regexp.MustCompile(`Label:\s+(?:'([^']*)'|(\S+))\s+uuid:\s+(\S+)`)
	mdadmArrayRe   = regexp.MustCompile(`^(md\d+)\s*:\s*(\w+)`)
	mdadmStatusRe  = regexp.MustCompile(`\[([U_]+)\]`)
	zpoolNoPoolsRe = regexp.MustCompile(`(?i)no pools available`)
	btrfsNoneRe    = regexp.MustCompile(`(?i)no btrfs|no valid btrfs|can't read`)
)

// probePools 依次尝试 btrfs / zpool / mdadm，汇总存储池状态。
func (p *sysProvider) probePools(ctx context.Context) ([]NASPool, CapStatus) {
	var pools []NASPool
	var errs int

	// btrfs：优先 CLI（可识别 degraded），无 CLI 时以已挂载 btrfs 兜底（只能判 ok）。
	if _, err := p.lookPath("btrfs"); err == nil {
		if bp, err := p.btrfsPools(ctx); err != nil {
			if !btrfsNoneRe.MatchString(err.Error()) {
				errs++
			}
		} else {
			pools = append(pools, bp...)
		}
	} else if bp := p.btrfsMountPools(ctx); len(bp) > 0 {
		pools = append(pools, bp...)
	}

	// zpool。
	if _, err := p.lookPath("zpool"); err == nil {
		if zp, err := p.zpoolPools(ctx); err != nil {
			if !zpoolNoPoolsRe.MatchString(err.Error()) {
				errs++
			}
		} else {
			pools = append(pools, zp...)
		}
	}

	// mdadm（仅 Linux 构造启用）。
	if p.enableMdadm {
		if mp, err := p.mdadmPools(); err != nil {
			errs++
		} else {
			pools = append(pools, mp...)
		}
	}

	switch {
	case len(pools) > 0:
		return pools, CapOK
	case errs > 0:
		return nil, CapFailed
	default:
		return nil, CapUnsupported
	}
}

// btrfsPools 解析 `btrfs filesystem show` 文本输出。
func (p *sysProvider) btrfsPools(ctx context.Context) ([]NASPool, error) {
	out, err := p.runner.Run(ctx, "btrfs", "filesystem", "show")
	if err != nil {
		return nil, wrapRunErr("btrfs filesystem show", out, err)
	}
	return parseBtrfsShow(string(out)), nil
}

// parseBtrfsShow 解析：
//
//	Label: 'storage1'  uuid: 550e8400-…
//		Total devices 2 FS bytes used …
//		devid 1 size … path /dev/sda3
//		*** Some devices missing
func parseBtrfsShow(out string) []NASPool {
	var pools []NASPool
	var cur *NASPool
	flush := func() {
		if cur != nil {
			pools = append(pools, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if m := btrfsLabelRe.FindStringSubmatch(line); m != nil {
			flush()
			name := m[1]
			if name == "" || name == "none" {
				name = m[3]
			}
			cur = &NASPool{Name: name, State: "ok"}
			continue
		}
		if cur != nil && strings.Contains(line, "Some devices missing") {
			cur.State = "degraded"
		}
	}
	flush()
	return pools
}

// btrfsMountPools 兜底：无 btrfs CLI 时把已挂载 btrfs 文件系统报为池（只能判 ok）。
func (p *sysProvider) btrfsMountPools(ctx context.Context) []NASPool {
	var pools []NASPool
	for _, d := range collectDisks(ctx) {
		if d.Fstype == "btrfs" {
			pools = append(pools, NASPool{Name: d.Mount, State: "ok"})
		}
	}
	return pools
}

// zpoolPools 解析 `zpool list -H -o name,health`。
func (p *sysProvider) zpoolPools(ctx context.Context) ([]NASPool, error) {
	out, err := p.runner.Run(ctx, "zpool", "list", "-H", "-o", "name,health")
	if err != nil {
		return nil, wrapRunErr("zpool list", out, err)
	}
	return parseZpoolList(string(out)), nil
}

// parseZpoolList：每行 "<name>\t<health>"。
func parseZpoolList(out string) []NASPool {
	var pools []NASPool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pools = append(pools, NASPool{Name: f[0], State: zpoolHealth(f[1])})
	}
	return pools
}

func zpoolHealth(h string) string {
	switch strings.ToUpper(h) {
	case "ONLINE":
		return "ok"
	case "DEGRADED":
		return "degraded"
	case "FAULTED", "UNAVAIL", "REMOVED", "SUSPENDED":
		return "error"
	default:
		return "unknown"
	}
}

// mdadmPools 解析 /proc/mdstat。
func (p *sysProvider) mdadmPools() ([]NASPool, error) {
	b, err := p.readFile("/proc/mdstat")
	if err != nil {
		return nil, nil // 无 mdstat 视为无 mdadm（不报错）
	}
	return parseMdstat(string(b)), nil
}

// parseMdstat：
//
//	md0 : active raid1 sda1[0] sdb1[1]
//	      976 blocks [2/2] [UU]
//	md1 : inactive sda2[0](S)
func parseMdstat(content string) []NASPool {
	var pools []NASPool
	var cur *NASPool
	flush := func() {
		if cur != nil {
			pools = append(pools, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if m := mdadmArrayRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &NASPool{Name: m[1], State: "unknown"}
			switch strings.ToLower(m[2]) {
			case "active":
				cur.State = "ok" // 待 [UU] 行修正
			case "inactive":
				cur.State = "error"
			}
			continue
		}
		if cur != nil {
			if m := mdadmStatusRe.FindStringSubmatch(line); m != nil {
				if strings.Contains(m[1], "_") {
					cur.State = "degraded"
				} else if cur.State == "unknown" {
					cur.State = "ok"
				}
			}
		}
	}
	flush()
	return pools
}

// wrapRunErr 把子进程错误连同已捕获输出包装（供"无池/无设备"判定）。
func wrapRunErr(what string, out []byte, err error) error {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return err
	}
	return &runError{what: what, output: msg, err: err}
}

type runError struct {
	what   string
	output string
	err    error
}

func (e *runError) Error() string {
	return e.what + ": " + e.err.Error() + ": " + e.output
}

func (e *runError) Unwrap() error { return e.err }
