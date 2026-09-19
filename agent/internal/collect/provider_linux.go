//go:build linux

package collect

// New 构造 Linux 采集 provider（启用 mdadm 等仅 Linux 探测）。
func New(cfg ProviderConfig) Provider {
	return newSysProvider(cfg, true)
}
