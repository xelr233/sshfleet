package exec

import (
	"context"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

func newTestPool(t *testing.T) (*pool.Pool, *sshtest.Server) {
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
		t.Fatalf("主机配置: %v", err)
	}
	p := pool.New(cfg, pool.Options{})
	t.Cleanup(func() { p.Close() })
	return p, srv
}

func TestRunCapturesStdoutAndExitCode(t *testing.T) {
	p, _ := newTestPool(t)
	res, err := Run(context.Background(), p, "h1", "hello", 0, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "echo:hello\n" {
		t.Fatalf("stdout 不符: %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("退出码应为 0, got %d", res.ExitCode)
	}
	if res.Duration < 0 || res.Duration > 5*time.Second {
		t.Fatalf("耗时不合理: %s", res.Duration)
	}
}

func TestRunNonZeroExitIsNotError(t *testing.T) {
	p, _ := newTestPool(t)
	// 非零退出码是命令结果,不是连接层错误:必须返回 (res, nil)。
	res, err := Run(context.Background(), p, "h1", "exit 7", 0, "")
	if err != nil {
		t.Fatalf("非零退出码不应作为 error 返回: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("退出码应为 7, got %d", res.ExitCode)
	}
	if res.Err != nil {
		t.Fatalf("Res.Err 应为 nil, got %v", res.Err)
	}
}

func TestRunSeparatesStderr(t *testing.T) {
	p, _ := newTestPool(t)
	res, err := Run(context.Background(), p, "h1", "stderr", 0, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "out\n" {
		t.Fatalf("stdout 不符: %q", res.Stdout)
	}
	if res.Stderr != "err\n" {
		t.Fatalf("stderr 不符: %q", res.Stderr)
	}
}

// 单机超时必须生效:远端不返回时,Run 应按 timeout 返回错误而非挂起。
func TestRunTimeout(t *testing.T) {
	p, _ := newTestPool(t)
	start := time.Now()
	_, err := Run(context.Background(), p, "h1", "sleep", 300*time.Millisecond, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时应及时生效,实际耗时 %s", elapsed)
	}
}

// ctx 取消同样应解除阻塞。
func TestRunContextCancel(t *testing.T) {
	p, _ := newTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, p, "h1", "sleep", 0, ""); err == nil {
		t.Fatal("ctx 取消应返回错误")
	}
}

// 未知 shell 应在建会话前被拒绝。
func TestRunUnknownShell(t *testing.T) {
	p, _ := newTestPool(t)
	if _, err := Run(context.Background(), p, "h1", "hi", 0, "fish"); err == nil {
		t.Fatal("未知 shell 应报错")
	}
}

// 远端 GBK 输出应被兜底解码为 UTF-8(README §5.9)。
func TestRunDecodesGBKOutput(t *testing.T) {
	p, _ := newTestPool(t)
	res, err := Run(context.Background(), p, "h1", "gbk", 0, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "中文\n" {
		t.Fatalf("GBK 输出应解码为 UTF-8 中文, got %q", res.Stdout)
	}
}
