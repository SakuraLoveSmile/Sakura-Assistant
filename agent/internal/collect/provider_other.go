//go:build !linux

package collect

// New 构造非 Linux 平台的降级 provider。
//
// 部署目标只有 Linux；此实现供开发机（macOS）编译与 --check 冒烟：
// 基础指标走 gopsutil 可用部分，docker / smartctl / 存储池探测照常执行
// （工具不存在时自然报 unsupported/failed），mdadm（/proc/mdstat）关闭。
func New(cfg ProviderConfig) Provider {
	return newSysProvider(cfg, false)
}
