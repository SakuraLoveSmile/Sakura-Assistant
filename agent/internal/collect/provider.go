package collect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// sysProvider 为真实采集实现。
//
// 平台说明：代码本身跨平台可编译（gopsutil + HTTP + 子进程探测均优雅降级），
// 但语义按 Linux 目标平台设计——Linux 构造（provider_linux.go，build tag linux）
// 额外启用 mdadm 等仅 Linux 的探测；非 Linux 构造走 provider_other.go，
// 供开发机编译与 --check 冒烟（NAS 能力自然报 unsupported/failed）。
type sysProvider struct {
	cfg         ProviderConfig
	runner      Runner
	lookPath    func(string) (string, error)
	readFile    func(string) ([]byte, error)
	listDir     func(string) ([]string, error)
	enableMdadm bool

	docker    *DockerClient
	dockerErr error

	mu   sync.Mutex
	prev counterState
}

func newSysProvider(cfg ProviderConfig, enableMdadm bool) *sysProvider {
	p := &sysProvider{
		cfg:         cfg,
		runner:      cfg.Runner,
		lookPath:    cfg.LookPath,
		readFile:    cfg.ReadFile,
		listDir:     cfg.ListDir,
		enableMdadm: enableMdadm,
	}
	if p.runner == nil {
		p.runner = execRunner{timeout: 10 * time.Second}
	}
	if p.lookPath == nil {
		p.lookPath = exec.LookPath
	}
	if p.readFile == nil {
		p.readFile = os.ReadFile
	}
	if p.listDir == nil {
		p.listDir = defaultListDir
	}
	p.docker, p.dockerErr = NewDockerClient(cfg.DockerHost)

	// 预热计数器：首次 Collect 即有真实差值（净速率 / CPU 利用率）。
	p.prev = readCounters(context.Background())
	return p
}

// defaultListDir 列目录项名（/sys/block 用）。
func defaultListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// Collect 采集一帧样本。基础指标 / 容器 / NAS 三块并行（互不写对方字段），
// 每块内部再有各自的子超时，保证整体不会卡过上报周期。
func (p *sysProvider) Collect(ctx context.Context) (*Result, error) {
	res := &Result{}
	res.Sample.TS = NewISOTime(time.Now())

	prev := p.swapCounters(ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.collectContainers(ctx, res)
	}()
	go func() {
		defer wg.Done()
		p.collectNAS(ctx, res)
	}()
	res.Capabilities.Network = p.collectBasics(ctx, prev, &res.Sample)
	wg.Wait()
	return res, nil
}

// swapCounters 读取当前计数器，与上次快照交换并返回旧值。
func (p *sysProvider) swapCounters(ctx context.Context) counterState {
	cur := readCounters(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.prev
	p.prev = cur
	return prev
}

// collectContainers 经 Docker API 列容器；socket 不存在 → unsupported；请求失败 → failed。
func (p *sysProvider) collectContainers(ctx context.Context, res *Result) {
	if p.dockerErr != nil || p.docker == nil {
		res.Capabilities.Containers = CapUnsupported
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cs, err := p.docker.Containers(cctx)
	if err != nil {
		if errors.Is(err, ErrDockerAbsent) {
			res.Capabilities.Containers = CapUnsupported
		} else {
			res.Capabilities.Containers = CapFailed
		}
		return
	}
	res.Capabilities.Containers = CapOK
	res.Sample.Containers = &cs
}

// collectNAS 并行探测 SMART 与存储池；任一侧有内容才输出 nas 块。
func (p *sysProvider) collectNAS(ctx context.Context, res *Result) {
	var wg sync.WaitGroup
	var disks []NASDisk
	var smartCap CapStatus
	var pools []NASPool
	var poolCap CapStatus
	wg.Add(2)
	go func() {
		defer wg.Done()
		disks, smartCap = p.probeSmart(ctx)
	}()
	go func() {
		defer wg.Done()
		pools, poolCap = p.probePools(ctx)
	}()
	wg.Wait()
	res.Capabilities.Smart = smartCap
	res.Capabilities.StoragePool = poolCap
	if len(disks) > 0 || len(pools) > 0 {
		res.Sample.NAS = &NAS{Disks: disks, Pools: pools}
	}
}
