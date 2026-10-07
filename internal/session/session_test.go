package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// newManager 起一个测试 SSH 服务器并返回会话管理器。
func newManager(t *testing.T) (*Manager, *pool.Pool) {
	t.Helper()
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动服务器: %v", err)
	}
	t.Cleanup(srv.Close)

	cfg := config.New()
	if err := cfg.Add(&config.Host{
		Name: "h1", Addr: srv.Host(), Port: srv.Port(),
		User: sshtest.User, Password: sshtest.Password,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(cfg, pool.Options{})
	t.Cleanup(func() { p.Close() })
	return NewManager(), p
}

// collect 订阅会话输出并在 d 内累积,返回最终字符串。
func collect(t *testing.T, s *Session, d time.Duration) func() string {
	t.Helper()
	var mu sync.Mutex
	var b strings.Builder
	cancel := s.Subscribe(func(p []byte) {
		mu.Lock()
		b.Write(p)
		mu.Unlock()
	})
	return func() string {
		defer cancel()
		time.Sleep(d)
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

// §5.6 / §9 M4 验收:cd 之后 pwd 反映新目录 —— 证明状态在同一 shell 会话中延续。
func TestSessionStatePersistsAcrossSends(t *testing.T) {
	m, p := newManager(t)
	conn, err := p.Get(context.Background(), "h1")
	if err != nil {
		t.Fatalf("取连接: %v", err)
	}
	s, err := m.Open(context.Background(), conn, 80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// 先 cd /tmp
	done1 := collect(t, s, 300*time.Millisecond)
	if err := s.Write("cd /tmp\n"); err != nil {
		t.Fatalf("写入 cd: %v", err)
	}
	if out := done1(); !strings.Contains(out, "cd ok -> /tmp") {
		t.Fatalf("cd 输出不符: %q", out)
	}

	// 再 pwd,应反映上一步的 cd(状态延续)
	done2 := collect(t, s, 300*time.Millisecond)
	if err := s.Write("pwd\n"); err != nil {
		t.Fatalf("写入 pwd: %v", err)
	}
	out := done2()
	if !strings.Contains(out, "/tmp") {
		t.Fatalf("pwd 应反映 cd 后的目录 /tmp, got %q", out)
	}
}

func TestSessionListAndClose(t *testing.T) {
	m, p := newManager(t)
	conn, _ := p.Get(context.Background(), "h1")
	s, err := m.Open(context.Background(), conn, 80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := s.ID()

	if got := m.List(); len(got) != 1 || got[0].ID != id {
		t.Fatalf("List 应含唯一会话 %s, got %+v", id, got)
	}
	if err := m.Close(id); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 关闭后应从表中移除(等待生命周期 goroutine 收尾)。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.List()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := m.List(); len(got) != 0 {
		t.Fatalf("关闭后应无会话, got %+v", got)
	}
}

func TestSessionCloseUnknown(t *testing.T) {
	m, _ := newManager(t)
	if err := m.Close("sess-999"); err == nil {
		t.Fatal("关闭不存在会话应报错")
	}
}

// 每个会话占用一个 channel 名额,关闭后归还(不泄漏)——
// 依次开满 channel 上限即可验证。
func TestSessionReleasesChannelOnClose(t *testing.T) {
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	cfg := config.New()
	_ = cfg.Add(&config.Host{Name: "h1", Addr: srv.Host(), Port: srv.Port(), User: sshtest.User, Password: sshtest.Password})
	p := pool.New(cfg, pool.Options{MaxChannels: 1})
	defer p.Close()

	m := NewManager()
	conn, _ := p.Get(context.Background(), "h1")

	for i := 0; i < 3; i++ {
		s, err := m.Open(context.Background(), conn, 80, 24)
		if err != nil {
			t.Fatalf("第 %d 次 Open(上限 1)不应失败: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}
