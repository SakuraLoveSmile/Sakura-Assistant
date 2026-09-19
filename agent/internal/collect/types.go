// Package collect 负责设备指标采集。
//
// 契约依据（均已冻结）：
//   - contracts/api-v1.md §2 ingest/metrics 载荷
//   - 采集不到的字段一律缺席（omitempty），绝不伪造为 0 或正常值。
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// CapStatus 是 capabilities 中每项能力的取值。
type CapStatus string

const (
	CapOK          CapStatus = "ok"
	CapAsleep      CapStatus = "asleep" // smart 专用：有休眠盘未探测
	CapUnsupported CapStatus = "unsupported"
	CapFailed      CapStatus = "failed"
)

// Capabilities 为本批次探测能力快照，键集合固定；中枢容忍未知键。
type Capabilities struct {
	Containers  CapStatus `json:"containers"`
	Smart       CapStatus `json:"smart"`
	StoragePool CapStatus `json:"storagePool"`
	Network     CapStatus `json:"network"`
}

// ISOTime 按契约以 RFC3339 UTC 毫秒精度编码（如 2026-09-18T06:29:58.000Z）。
type ISOTime struct {
	time.Time
}

// NewISOTime 归一化为 UTC 毫秒。
func NewISOTime(t time.Time) ISOTime {
	return ISOTime{Time: t.UTC().Truncate(time.Millisecond)}
}

// MarshalJSON 输出固定毫秒精度 RFC3339。
func (t ISOTime) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", t.UTC().Format("2006-01-02T15:04:05.000Z"))), nil
}

// UnmarshalJSON 兼容 RFC3339 / RFC3339Nano。
func (t *ISOTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// DiskUsage 为一个挂载点的用量。
type DiskUsage struct {
	Mount      string  `json:"mount"`
	Percent    float64 `json:"percent"`
	UsedBytes  uint64  `json:"usedBytes"`
	TotalBytes uint64  `json:"totalBytes"`
	Fstype     string  `json:"fstype"`
}

// NetRate 为网络字节速率（计数器差值 / 间隔）。
type NetRate struct {
	RxBps uint64 `json:"rxBps"`
	TxBps uint64 `json:"txBps"`
}

// Container 为一个受管容器的状态。
type Container struct {
	Name         string   `json:"name"`
	State        string   `json:"state"` // running|exited|restarting|paused|dead|created
	ExitCode     *int     `json:"exitCode"`
	StartedAt    *ISOTime `json:"startedAt,omitempty"`
	RestartCount int      `json:"restartCount"`
}

// NASDisk 为 NAS 盘 SMART 状态。
type NASDisk struct {
	Dev   string `json:"dev"`
	Smart string `json:"smart"` // ok|failing|asleep|unsupported|failed
	TempC *int   `json:"tempC,omitempty"`
}

// NASPool 为存储池状态。
type NASPool struct {
	Name  string `json:"name"`
	State string `json:"state"` // ok|degraded|error|unknown
}

// NAS 为飞牛来源附加块；capabilities 指示支持度。
type NAS struct {
	Disks []NASDisk `json:"disks,omitempty"`
	Pools []NASPool `json:"pools,omitempty"`
}

// Sample 为一帧指标样本。指针字段 nil → 缺席（采不到）。
type Sample struct {
	TS            ISOTime      `json:"ts"`
	CPUPercent    *float64     `json:"cpuPercent,omitempty"`
	MemPercent    *float64     `json:"memPercent,omitempty"`
	MemUsedBytes  *uint64      `json:"memUsedBytes,omitempty"`
	MemTotalBytes *uint64      `json:"memTotalBytes,omitempty"`
	Load1         *float64     `json:"load1,omitempty"`
	UptimeSeconds *uint64      `json:"uptimeSeconds,omitempty"`
	BootTime      *ISOTime     `json:"bootTime,omitempty"`
	Disks         []DiskUsage  `json:"disks,omitempty"`
	Net           *NetRate     `json:"net,omitempty"`
	Containers    *[]Container `json:"containers,omitempty"` // cap=ok 时全量列出（可为空数组）
	NAS           *NAS         `json:"nas,omitempty"`
}

// Result 为一次采集结果：能力快照 + 样本。
type Result struct {
	Capabilities Capabilities
	Sample       Sample
}

// Provider 抽象指标采集，测试用 fake 实现。
type Provider interface {
	Collect(ctx context.Context) (*Result, error)
}

// Runner 抽象子进程执行，smartctl / btrfs / zpool 探测走它，测试可注入 fake。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ProviderConfig 为 provider 的可选依赖（测试注入用；零值走真实系统）。
type ProviderConfig struct {
	DockerHost string                         // unix:///var/run/docker.sock | tcp://… | http(s)://…
	SmartMode  string                         // auto | on | off
	Runner     Runner                         // 子进程执行器；nil → execRunner{10s}
	LookPath   func(string) (string, error)   // nil → exec.LookPath
	ReadFile   func(string) ([]byte, error)   // nil → os.ReadFile（/proc/mdstat 用）
	ListDir    func(string) ([]string, error) // nil → 列目录项名（/sys/block 用）
}
