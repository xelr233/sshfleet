package dist

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// testHost 是一台「机器」:一个测试 SSH 服务器及其 SFTP 根目录。
type testHost struct {
	name string
	root string
	srv  *sshtest.Server
}

// newDistEnv 起 n 台机器,返回连接池、机器列表与清理由 t.Cleanup 负责。
// throttle 可取任意非负速率(字节/秒),对所有机器统一施加;为 0 表示不限速。
func newDistEnv(t *testing.T, n int, throttle int64) (*pool.Pool, []testHost) {
	t.Helper()
	cfg := config.New()
	hosts := make([]testHost, 0, n)
	for i := 0; i < n; i++ {
		srv, err := sshtest.Start()
		if err != nil {
			t.Fatalf("启动服务器 %d: %v", i, err)
		}
		t.Cleanup(srv.Close)
		srv.Root = t.TempDir()
		if throttle > 0 {
			srv.Throttle = &sshtest.Throttle{BytesPerSec: throttle}
		}
		name := string(rune('a' + i))
		if err := cfg.Add(&config.Host{
			Name: name, Addr: srv.Host(), Port: srv.Port(),
			User: sshtest.User, Password: sshtest.Password,
		}); err != nil {
			t.Fatalf("主机配置: %v", err)
		}
		hosts = append(hosts, testHost{name: name, root: srv.Root, srv: srv})
	}
	p := pool.New(cfg, pool.Options{MaxChannels: 8})
	t.Cleanup(func() { p.Close() })
	return p, hosts
}

func writeLocal(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func hostNames(hosts []testHost) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.name)
	}
	return out
}

// 基本分发:每台机器都拿到完整、字节一致的文件,状态为 done。
func TestDistributeToAllHosts(t *testing.T) {
	p, hosts := newDistEnv(t, 3, 0)
	path, data := writeLocal(t, 2*ChunkSize+4096)

	task := NewTask("t1", hostNames(hosts), Options{
		LocalPath: path, RemotePath: "out/file.bin", Mkdir: true,
	})
	task.Run(p)
	res := task.Wait()

	if res.Canceled {
		t.Fatal("不应被取消")
	}
	if len(res.Hosts) != 3 {
		t.Fatalf("结果主机数应为 3, got %d", len(res.Hosts))
	}
	for _, h := range hosts {
		got, err := os.ReadFile(filepath.Join(h.root, "out", "file.bin"))
		if err != nil {
			t.Fatalf("读 %s 的分发结果: %v", h.name, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("主机 %s 内容不一致(len=%d want=%d)", h.name, len(got), len(data))
		}
	}
	for _, hp := range res.Hosts {
		if hp.State != StateDone {
			t.Fatalf("主机 %s 应为 done, got %s (%s)", hp.Host, hp.State, hp.Err)
		}
		if hp.Offset != int64(len(data)) || hp.Total != int64(len(data)) {
			t.Fatalf("主机 %s 进度应为满, got %d/%d", hp.Host, hp.Offset, hp.Total)
		}
	}
}

// README §9 M3 验收:慢机不阻塞快机。
// 一台限速机与两台不限速机同时分发:快机必须显著早于慢机完成。
func TestSlowHostDoesNotBlockOthers(t *testing.T) {
	p, hosts := newDistEnv(t, 3, 0)
	// 让最后一台机器限速(2 MiB/s)。
	slow := &hosts[2]
	slow.srv.Throttle = &sshtest.Throttle{BytesPerSec: 2 << 20}

	path, data := writeLocal(t, 6<<20) // 6 MiB:慢机约需 ~3s

	type stamp struct {
		done bool
		at   time.Duration
	}
	var mu sync.Mutex
	stamps := map[string]stamp{}
	start := time.Now()

	task := NewTask("t2", hostNames(hosts), Options{
		LocalPath: path, RemotePath: "f.bin", Mkdir: true,
		OnProgress: func(hp HostProgress) {
			if hp.State != StateDone {
				return
			}
			mu.Lock()
			if _, seen := stamps[hp.Host]; !seen {
				stamps[hp.Host] = stamp{done: true, at: time.Since(start)}
			}
			mu.Unlock()
		},
	})
	task.Run(p)
	res := task.Wait()
	if res.Canceled {
		t.Fatal("不应被取消")
	}

	// 所有机器最终都要拿到完整文件。
	for _, h := range hosts {
		got, err := os.ReadFile(filepath.Join(h.root, "f.bin"))
		if err != nil {
			t.Fatalf("读 %s 结果: %v", h.name, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("主机 %s 内容不一致", h.name)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	slowDone, ok := stamps[slow.name]
	if !ok {
		t.Fatal("慢机未上报完成")
	}
	// 快机在慢机完成之前就应完成(慢机耗时约 3s,快机应远早于此)。
	for _, h := range hosts[:2] {
		st, ok := stamps[h.name]
		if !ok {
			t.Fatalf("快机 %s 未上报完成", h.name)
		}
		if st.at >= slowDone.at {
			t.Fatalf("快机 %s 完成于 %s,不早于慢机 %s(慢机未阻塞快机)", h.name, st.at, slowDone.at)
		}
	}
	if slowDone.at < 500*time.Millisecond {
		t.Fatalf("慢机耗时 %s 与限速不符(疑似未限速)", slowDone.at)
	}
}

// 续传:远端已有前半段时,Offset 从已有大小开始,最终内容完整不重复。
func TestResumeFromPartial(t *testing.T) {
	p, hosts := newDistEnv(t, 3, 0)
	path, data := writeLocal(t, 4*ChunkSize) // 4 块

	// 在三台机器的目标路径上各写入一部分(模拟上次中断)。
	partial := int64(ChunkSize + 1000)
	for _, h := range hosts {
		dst := filepath.Join(h.root, "r.bin")
		if err := os.WriteFile(dst, data[:partial], 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	startOffsets := map[string]int64{}
	task := NewTask("t3", hostNames(hosts), Options{
		LocalPath: path, RemotePath: "r.bin", Resume: true,
		OnProgress: func(hp HostProgress) {
			if hp.State != StateRunning {
				return
			}
			mu.Lock()
			if _, seen := startOffsets[hp.Host]; !seen {
				startOffsets[hp.Host] = hp.Offset
			}
			mu.Unlock()
		},
	})
	task.Run(p)
	task.Wait()

	for _, h := range hosts {
		got, err := os.ReadFile(filepath.Join(h.root, "r.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("主机 %s 续传后内容不一致(len=%d want=%d)", h.name, len(got), len(data))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range hosts {
		if got := startOffsets[h.name]; got != partial {
			t.Fatalf("主机 %s 续传起点应为 %d, got %d", h.name, partial, got)
		}
	}
}

// 取消:传输中途 Cancel,任务标记为 canceled,且未传完。
func TestCancelMidTransfer(t *testing.T) {
	p, hosts := newDistEnv(t, 1, 0)
	hosts[0].srv.Throttle = &sshtest.Throttle{BytesPerSec: 1 << 20} // 1 MiB/s

	path, _ := writeLocal(t, 8<<20) // 需要约 8s
	task := NewTask("t4", hostNames(hosts), Options{
		LocalPath: path, RemotePath: "c.bin",
	})

	done := make(chan struct{})
	go func() { task.Run(p); close(done) }()

	time.Sleep(300 * time.Millisecond)
	task.Cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后任务未及时结束")
	}
	res := task.Wait()
	if !res.Canceled {
		t.Fatal("取消后 Result.Canceled 应为 true")
	}
	if res.Hosts[0].State != StateFailed {
		t.Fatalf("取消机器应标记 failed, got %s", res.Hosts[0].State)
	}
}

// 源文件不存在:所有机器标记失败,任务正常结束而非挂起。
func TestSourceMissingFailsAll(t *testing.T) {
	p, hosts := newDistEnv(t, 2, 0)
	task := NewTask("t5", hostNames(hosts), Options{
		LocalPath: filepath.Join(t.TempDir(), "nope.bin"), RemotePath: "x.bin",
	})
	task.Run(p)
	res := task.Wait()
	for _, hp := range res.Hosts {
		if hp.State != StateFailed {
			t.Fatalf("主机 %s 应失败, got %s", hp.Host, hp.State)
		}
	}
}

// Progress 快照接口可随时查询(daemon dist_status 依赖它)。
func TestProgressSnapshot(t *testing.T) {
	p, hosts := newDistEnv(t, 2, 0)
	path, data := writeLocal(t, 1<<20)
	task := NewTask("t6", hostNames(hosts), Options{
		LocalPath: path, RemotePath: "s.bin", Mkdir: true,
	})
	task.Run(p)
	task.Wait()

	state, hps, doneCount, failed := task.Progress()
	if state != "done" || doneCount != 2 || failed != 0 {
		t.Fatalf("进度快照不符: state=%s done=%d failed=%d", state, doneCount, failed)
	}
	for _, hp := range hps {
		if hp.Offset != int64(len(data)) {
			t.Fatalf("主机 %s offset=%d want=%d", hp.Host, hp.Offset, len(data))
		}
	}
}

// 空任务(无目标)不应 panic 或挂起。
func TestNoHosts(t *testing.T) {
	p, _ := newDistEnv(t, 1, 0)
	path, _ := writeLocal(t, 1024)
	task := NewTask("t7", nil, Options{LocalPath: path, RemotePath: "y.bin"})
	task.Run(p)
	res := task.Wait()
	if len(res.Hosts) != 0 {
		t.Fatalf("无目标应无结果, got %d", len(res.Hosts))
	}
}
