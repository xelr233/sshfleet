package pool

import (
	"context"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// newTestPool 启动一个进程内 SSH 服务器并返回指向它的连接池。
func newTestPool(t *testing.T, opts Options) (*Pool, *sshtest.Server) {
	t.Helper()
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动测试 SSH 服务器: %v", err)
	}
	t.Cleanup(srv.Close)

	cfg := config.New()
	if err := cfg.Add(&config.Host{
		Name: "h1", Addr: srv.Host(), Port: srv.Port(),
		User: sshtest.User, Password: sshtest.Password,
	}); err != nil {
		t.Fatalf("构造主机配置: %v", err)
	}

	p := New(cfg, opts)
	t.Cleanup(func() { p.Close() })
	return p, srv
}

// 连续多次 Get 必须复用同一条连接,只发生一次握手(README §9 M1 验收)。
func TestPoolReusesConnection(t *testing.T) {
	p, srv := newTestPool(t, Options{})
	ctx := context.Background()

	first, err := p.Get(ctx, "h1")
	if err != nil {
		t.Fatalf("首次 Get: %v", err)
	}
	for i := 0; i < 20; i++ {
		c, err := p.Get(ctx, "h1")
		if err != nil {
			t.Fatalf("第 %d 次 Get: %v", i, err)
		}
		if c != first {
			t.Fatalf("第 %d 次 Get 返回了不同的连接,连接未被复用", i)
		}
	}
	if h := srv.Handshakes(); h != 1 {
		t.Fatalf("21 次 Get 应只握手 1 次, got %d", h)
	}
}

// 并发 Get 必须被 singleflight 去重,不产生惊群(README §8)。
func TestPoolConcurrentGetDeduplicates(t *testing.T) {
	p, srv := newTestPool(t, Options{})
	ctx := context.Background()

	const n = 16
	errs := make(chan error, n)
	conns := make(chan *Conn, n)
	for i := 0; i < n; i++ {
		go func() {
			c, err := p.Get(ctx, "h1")
			errs <- err
			conns <- c
		}()
	}
	var first *Conn
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发 Get 失败: %v", err)
		}
		c := <-conns
		if first == nil {
			first = c
		} else if c != first {
			t.Fatal("并发 Get 返回了不同连接")
		}
	}
	if h := srv.Handshakes(); h != 1 {
		t.Fatalf("16 个并发 Get 应只握手 1 次, got %d", h)
	}
}

// 服务器关闭后连接应被标记失效;服务器恢复后下次 Get 应自动重连并计数。
func TestPoolAutoReconnect(t *testing.T) {
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动服务器: %v", err)
	}
	addr := srv.Addr()

	cfg := config.New()
	_ = cfg.Add(&config.Host{
		Name: "h1", Addr: srv.Host(), Port: srv.Port(),
		User: sshtest.User, Password: sshtest.Password,
	})
	p := New(cfg, Options{DialTimeout: 3 * time.Second, BackoffBase: 10 * time.Millisecond, BackoffMax: 50 * time.Millisecond})
	defer p.Close()
	ctx := context.Background()

	if _, err := p.Get(ctx, "h1"); err != nil {
		t.Fatalf("首次连接: %v", err)
	}
	srv.Close() // 模拟断线

	// 断开后连接应被判失效(等待 watch 感知)。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stats := p.Stats(); len(stats) == 1 && !stats[0].Connected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stats := p.Stats(); stats[0].Connected {
		t.Fatal("服务器关闭后连接仍显示 connected")
	}

	// 在同一地址重启服务器,下次 Get 应自动重连。
	srv2, err := sshtest.StartAt(addr)
	if err != nil {
		t.Fatalf("同地址重启服务器: %v", err)
	}
	defer srv2.Close()

	c, err := p.Get(ctx, "h1")
	if err != nil {
		t.Fatalf("重连 Get: %v", err)
	}
	if c == nil || c.Closed() {
		t.Fatal("重连后应得到可用连接")
	}
	if stats := p.Stats(); stats[0].Reconnects < 1 {
		t.Fatalf("重连计数应 >= 1, got %d", stats[0].Reconnects)
	}
}

// 每主机并发 channel 数受 MaxChannels 限制(README §5.3)。
func TestPoolChannelLimit(t *testing.T) {
	p, _ := newTestPool(t, Options{MaxChannels: 2})
	ctx := context.Background()

	c, err := p.Get(ctx, "h1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// 占满 2 个名额。
	if err := c.Acquire(ctx); err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if err := c.Acquire(ctx); err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if got := c.InUse(); got != 2 {
		t.Fatalf("InUse 应为 2, got %d", got)
	}

	// 第 3 个应被阻塞,直到 ctx 超时。
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := c.Acquire(shortCtx); err == nil {
		t.Fatal("超过 channel 上限的 Acquire 应被阻塞并按 ctx 超时")
	}

	c.Release()
	if got := c.InUse(); got != 1 {
		t.Fatalf("Release 后 InUse 应为 1, got %d", got)
	}
}

// 未知主机必须返回明确错误。
func TestPoolUnknownHost(t *testing.T) {
	p, _ := newTestPool(t, Options{})
	if _, err := p.Get(context.Background(), "nope"); err == nil {
		t.Fatal("未知主机应报错")
	}
}

// 主机不可达时应在有限重试后返回错误,而不是无限挂起(README §5.8)。
func TestPoolDialFailureIsBounded(t *testing.T) {
	cfg := config.New()
	// 127.0.0.1:1 基本不可能有监听。
	_ = cfg.Add(&config.Host{Name: "dead", Addr: "127.0.0.1", Port: 1, User: "u", Password: "p"})
	p := New(cfg, Options{
		DialTimeout:     500 * time.Millisecond,
		BackoffBase:     10 * time.Millisecond,
		BackoffMax:      20 * time.Millisecond,
		MaxDialAttempts: 3,
	})
	defer p.Close()

	start := time.Now()
	_, err := p.Get(context.Background(), "dead")
	if err == nil {
		t.Fatal("不可达主机应报错")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("重试应快速失败,实际耗时 %s", elapsed)
	}
}
