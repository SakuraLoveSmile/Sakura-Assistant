package collect

import (
	"context"
	"errors"
	"testing"
)

const btrfsShowOut = `Label: 'storage1'  uuid: 550e8400-e29b-41d4-a716-446655440000
	Total devices 2 FS bytes used 1.20TiB
	devid    1 size 3.64TiB used 1.32TiB path /dev/sda3
	devid    2 size 3.64TiB used 1.32TiB path /dev/sdb3

Label: none  uuid: 660e8400-e29b-41d4-a716-446655440001
	Total devices 2 FS bytes used 2.00TiB
	devid    1 size 3.64TiB used 2.10TiB path /dev/sdc1
	*** Some devices missing
`

func TestParseBtrfsShow(t *testing.T) {
	pools := parseBtrfsShow(btrfsShowOut)
	if len(pools) != 2 {
		t.Fatalf("pools = %+v; want 2", pools)
	}
	if pools[0].Name != "storage1" || pools[0].State != "ok" {
		t.Fatalf("pool0 = %+v", pools[0])
	}
	if pools[1].Name != "660e8400-e29b-41d4-a716-446655440001" || pools[1].State != "degraded" {
		t.Fatalf("pool1 = %+v（none label→uuid，missing→degraded）", pools[1])
	}
}

func TestParseZpoolList(t *testing.T) {
	out := "tank\tONLINE\nstorage1\tDEGRADED\nbad\tFAULTED\n"
	pools := parseZpoolList(out)
	if len(pools) != 3 {
		t.Fatalf("pools = %+v", pools)
	}
	if pools[0].State != "ok" || pools[1].State != "degraded" || pools[2].State != "error" {
		t.Fatalf("states = %+v", pools)
	}
}

const mdstatOut = `Personalities : [raid1]
md0 : active raid1 sdb1[1] sda1[0]
      9767424 blocks [2/2] [UU]
      bitmap: 0/1 pages [0KB]

md1 : active raid1 sdc1[0] sdd1[1]
      1953266304 blocks super 1.2 [2/1] [U_]

md2 : inactive sde1[0](S)

unused devices: <none>
`

func TestParseMdstat(t *testing.T) {
	pools := parseMdstat(mdstatOut)
	if len(pools) != 3 {
		t.Fatalf("pools = %+v", pools)
	}
	if pools[0].Name != "md0" || pools[0].State != "ok" {
		t.Fatalf("md0 = %+v; want ok", pools[0])
	}
	if pools[1].Name != "md1" || pools[1].State != "degraded" {
		t.Fatalf("md1 = %+v; want degraded", pools[1])
	}
	if pools[2].Name != "md2" || pools[2].State != "error" {
		t.Fatalf("md2 = %+v; want error（inactive）", pools[2])
	}
}

// 聚合：无 btrfs/zpool CLI 且无 mdadm → unsupported；btrfs CLI 探测成功 → ok。
func TestProbePoolsAggregate(t *testing.T) {
	// 全无 → unsupported
	p := newSysProvider(ProviderConfig{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		ReadFile: func(string) ([]byte, error) { return []byte("Personalities :\nunused devices: <none>\n"), nil },
	}, true)
	pools, cap := p.probePools(context.Background())
	if cap != CapUnsupported || len(pools) != 0 {
		t.Fatalf("cap=%s pools=%+v; want unsupported/empty", cap, pools)
	}

	// btrfs CLI + zpool + mdadm 并存
	runner := &fakeRunner{outputs: map[string]fakeRunResult{
		"btrfs filesystem show":        {out: []byte(btrfsShowOut)},
		"zpool list -H -o name,health": {out: []byte("tank\tONLINE\n")},
	}}
	p = newSysProvider(ProviderConfig{
		Runner:   runner,
		LookPath: func(n string) (string, error) { return "/usr/sbin/" + n, nil },
		ReadFile: func(string) ([]byte, error) { return []byte(mdstatOut), nil },
	}, true)
	pools, cap = p.probePools(context.Background())
	if cap != CapOK {
		t.Fatalf("cap = %s; want ok", cap)
	}
	if len(pools) != 2+1+3 {
		t.Fatalf("pools = %+v; want btrfs2+zpool1+mdadm3", pools)
	}

	// zpool 报 "no pools available" → 不算错误
	runner2 := &fakeRunner{outputs: map[string]fakeRunResult{
		"zpool list -H -o name,health": {out: nil, err: errors.New("exit status 1: no pools available")},
		"btrfs filesystem show":        {out: nil, err: errors.New("exit status 1: No btrfs file system found")},
	}}
	p = newSysProvider(ProviderConfig{
		Runner:   runner2,
		LookPath: func(n string) (string, error) { return "/usr/sbin/" + n, nil },
		ReadFile: func(string) ([]byte, error) { return []byte("Personalities :\nunused devices: <none>\n"), nil },
	}, true)
	pools, cap = p.probePools(context.Background())
	if cap != CapUnsupported || len(pools) != 0 {
		t.Fatalf("cap=%s pools=%+v; want unsupported（无池非失败）", cap, pools)
	}

	// 探测出错 → failed
	runner3 := &fakeRunner{outputs: map[string]fakeRunResult{
		"btrfs filesystem show": {out: nil, err: errors.New("exit status 1: permission denied")},
	}}
	p = newSysProvider(ProviderConfig{
		Runner: runner3,
		LookPath: func(n string) (string, error) {
			if n == "btrfs" {
				return "/usr/sbin/btrfs", nil
			}
			return "", errors.New("nf")
		},
		ReadFile: func(string) ([]byte, error) { return []byte("Personalities :\nunused devices: <none>\n"), nil },
	}, true)
	_, cap = p.probePools(context.Background())
	if cap != CapFailed {
		t.Fatalf("cap = %s; want failed（权限不足）", cap)
	}
}
