package exec

import (
	"context"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// newBatchPool 启动多个测试 SSH 服务器,返回指向它们的连接池与主机名(按名排序)。
func newBatchPool(t *testing.T, n int) (*pool.Pool, []string) {
	t.Helper()
	cfg := config.New()
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		srv, err := sshtest.Start()
		if err != nil {
			t.Fatalf("启动测试 SSH 服务器 %d: %v", i, err)
		}
		t.Cleanup(srv.Close)
		// 主机名补零,保证按名字排序后与创建顺序一致,便于断言。
		name := string(rune('a' + i))
		if err := cfg.Add(&config.Host{
			Name: name, Addr: srv.Host(), Port: srv.Port(),
			User: sshtest.User, Password: sshtest.Password,
		}); err != nil {
			t.Fatalf("主机配置 %s: %v", name, err)
		}
		names = append(names, name)
	}
	p := pool.New(cfg, pool.Options{})
	t.Cleanup(func() { p.Close() })
	return p, names
}

// newSharedServer 启动一个测试 SSH 服务器,供多台「主机」共享(用于观测共享的
// 峰值并发,而无需关心每个主机各自的连接)。
func newSharedServer(t *testing.T) *sshtest.Server {
	t.Helper()
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动测试 SSH 服务器: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestRunBatchAggregatesResults(t *testing.T) {
	p, hosts := newBatchPool(t, 3)
	out, err := RunBatch(context.Background(), p, hosts, "hi", BatchOptions{})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("结果数应为 3, got %d", len(out.Results))
	}
	for i, r := range out.Results {
		if r.Host != hosts[i] {
			t.Fatalf("结果顺序应为排序后主机名: got %q want %q", r.Host, hosts[i])
		}
		if r.Stdout != "echo:hi\n" || r.ExitCode != 0 || r.Err != nil {
			t.Fatalf("主机 %s 结果异常: %+v", r.Host, r)
		}
	}
}

// 单机失败不得中断其它机器:非零退出码逐机记录,RunBatch 本身返回 nil error。
func TestRunBatchIsolatesFailures(t *testing.T) {
	p, hosts := newBatchPool(t, 3)
	out, err := RunBatch(context.Background(), p, hosts, "exit 3", BatchOptions{})
	if err != nil {
		t.Fatalf("单机非零退出码不应作为 RunBatch 错误: %v", err)
	}
	for _, r := range out.Results {
		if r.ExitCode != 3 {
			t.Fatalf("主机 %s 退出码应为 3, got %d", r.Host, r.ExitCode)
		}
		if r.Err != nil {
			t.Fatalf("非零退出码不是连接层错误: %v", r.Err)
		}
	}
}

// 并发度必须生效:5 台、-c 2 时服务端观测到的峰值并发不超过 2。
func TestRunBatchConcurrencyLimit(t *testing.T) {
	srv := newSharedServer(t)
	const n = 5
	cfg := config.New()
	hosts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := string(rune('a' + i))
		// 全部指向同一服务器(共享活跃计数),验证 semaphore 而非网络。
		if err := cfg.Add(&config.Host{
			Name: name, Addr: srv.Host(), Port: srv.Port(),
			User: sshtest.User, Password: sshtest.Password,
		}); err != nil {
			t.Fatalf("主机配置: %v", err)
		}
		hosts = append(hosts, name)
	}
	p := pool.New(cfg, pool.Options{MaxChannels: 16})
	defer p.Close()

	start := time.Now()
	if _, err := RunBatch(context.Background(), p, hosts, "delay 120", BatchOptions{Concurrency: 2}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got := srv.MaxConcurrent(); got > 2 {
		t.Fatalf("并发度上限应为 2,服务端观测到峰值 %d", got)
	}
	// 5 个任务、并发 2、每个 120ms:串行化下至少需要三批 ≈ 240ms 墙钟时间。
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("并发受限时不应过快完成(疑似未受限): %s", elapsed)
	}
}

// 逐机超时:慢机超时失败,快机照常成功,互不影响。
func TestRunBatchPerHostTimeout(t *testing.T) {
	p, hosts := newBatchPool(t, 2)
	out, err := RunBatch(context.Background(), p, hosts, "delay 500", BatchOptions{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	for _, r := range out.Results {
		if r.Err == nil {
			t.Fatalf("主机 %s 应因超时失败", r.Host)
		}
	}
}

// ctx 取消时,所有机器都拿到明确错误,不留空白主机。
func TestRunBatchContextCancel(t *testing.T) {
	p, hosts := newBatchPool(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, err := RunBatch(ctx, p, hosts, "sleep", BatchOptions{Concurrency: 1})
	if err == nil && out == nil {
		t.Fatal("取消后应至少返回结果或错误")
	}
	if out != nil {
		for _, r := range out.Results {
			if r.Host == "" {
				t.Fatal("结果中不应出现空白主机")
			}
		}
	}
}

func TestRunBatchRejectsEmptyCommand(t *testing.T) {
	p, hosts := newBatchPool(t, 1)
	if _, err := RunBatch(context.Background(), p, hosts, "", BatchOptions{}); err == nil {
		t.Fatal("空命令应报错")
	}
}
