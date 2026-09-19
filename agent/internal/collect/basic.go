package collect

import (
	"context"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// 计数器快照（CPU 时间、网络字节），用于跨采样差值计算速率 / 利用率。
type counterState struct {
	cpuTotal, cpuIdle float64
	cpuAt             time.Time
	netRx, netTx      uint64
	netAt             time.Time
}

// readCounters 读取一次计数器（尽力而为，单项失败不影响另一项）。
func readCounters(ctx context.Context) counterState {
	st := counterState{cpuAt: time.Now(), netAt: time.Now()}
	if ts, err := cpu.TimesWithContext(ctx, false); err == nil && len(ts) > 0 {
		st.cpuTotal = ts[0].Total()
		st.cpuIdle = ts[0].Idle + ts[0].Iowait
	}
	if ios, err := net.IOCountersWithContext(ctx, true); err == nil {
		for _, io := range ios {
			if isLoopback(io.Name) {
				continue
			}
			st.netRx += io.BytesRecv
			st.netTx += io.BytesSent
		}
	}
	return st
}

func isLoopback(name string) bool {
	return name == "lo" || strings.HasPrefix(name, "lo")
}

// collectBasics 采集 CPU / 内存 / load / uptime / bootTime / 磁盘 / 网络速率。
// 每一项独立尽力而为：失败 → 字段缺席。prev 为上次计数器快照。
func (p *sysProvider) collectBasics(ctx context.Context, prev counterState, s *Sample) CapStatus {
	netCap := CapOK
	now := time.Now()

	// CPU：累计时间差值 → 利用率。
	if ts, err := cpu.TimesWithContext(ctx, false); err == nil && len(ts) > 0 {
		total := ts[0].Total()
		idle := ts[0].Idle + ts[0].Iowait
		if prev.cpuTotal > 0 {
			dt := total - prev.cpuTotal
			db := dt - (idle - prev.cpuIdle)
			if dt > 0 && db >= 0 {
				v := round2(db / dt * 100)
				if v > 100 {
					v = 100
				}
				s.CPUPercent = &v
			}
		}
	}

	// 内存。
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		pct := round2(vm.UsedPercent)
		used, total := vm.Used, vm.Total
		s.MemPercent = &pct
		s.MemUsedBytes = &used
		s.MemTotalBytes = &total
	}

	// load1。
	if avg, err := load.AvgWithContext(ctx); err == nil {
		v := avg.Load1
		s.Load1 = &v
	}

	// uptime / bootTime。
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.UptimeSeconds = &up
	}
	if bt, err := host.BootTimeWithContext(ctx); err == nil && bt > 0 {
		t := NewISOTime(time.Unix(int64(bt), 0))
		s.BootTime = &t
	}

	// 磁盘：每挂载点。
	s.Disks = collectDisks(ctx)

	// 网络速率：计数器差值 / 实际间隔。
	if ios, err := net.IOCountersWithContext(ctx, true); err == nil {
		var rx, tx uint64
		for _, io := range ios {
			if isLoopback(io.Name) {
				continue
			}
			rx += io.BytesRecv
			tx += io.BytesSent
		}
		if prev.netAt.Unix() > 0 {
			dt := now.Sub(prev.netAt).Seconds()
			if dt > 0 {
				s.Net = &NetRate{
					RxBps: uint64(float64(rx-prev.netRx) / dt),
					TxBps: uint64(float64(tx-prev.netTx) / dt),
				}
			}
		}
	} else {
		netCap = CapFailed
	}
	return netCap
}

// collectDisks 列出物理分区（all=false）并逐挂载点取用量；statfs 失败的挂载点跳过。
func collectDisks(ctx context.Context) []DiskUsage {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []DiskUsage
	for _, pt := range parts {
		mount := pt.Mountpoint
		if mount == "" || seen[mount] {
			continue
		}
		seen[mount] = true
		u, err := disk.UsageWithContext(ctx, mount)
		if err != nil || u.Total == 0 {
			continue
		}
		out = append(out, DiskUsage{
			Mount:      mount,
			Percent:    round2(u.UsedPercent),
			UsedBytes:  u.Used,
			TotalBytes: u.Total,
			Fstype:     pt.Fstype,
		})
	}
	return out
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}
