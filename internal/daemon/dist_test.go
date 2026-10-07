package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/protocol"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// startDistDaemon 起一个 daemon,带 n 台指向各自 SFTP 根的测试机(组名 all)。
// 返回 daemon、各机 SFTP 根目录与主机名。
func startDistDaemon(t *testing.T, n int) (*Daemon, []string, map[string]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	var b strings.Builder
	b.WriteString("hosts:\n")
	roots := map[string]string{}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		srv, err := sshtest.Start()
		if err != nil {
			t.Fatalf("启动服务器: %v", err)
		}
		t.Cleanup(srv.Close)
		srv.Root = t.TempDir()
		name := string(rune('a' + i))
		names = append(names, name)
		roots[name] = srv.Root
		fmt.Fprintf(&b, "  %s:\n    addr: %s\n    port: %d\n    user: %s\n    password: %s\n",
			name, srv.Host(), srv.Port(), sshtest.User, sshtest.Password)
	}
	b.WriteString("groups:\n  all:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "    - %s\n", name)
	}

	cfgPath := filepath.Join(home, "hosts.yaml")
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 hosts.yaml: %v", err)
	}

	d := New(Options{Addr: "127.0.0.1:0", ConfigPath: cfgPath, Version: "test", IdleTimeout: time.Hour})
	if err := d.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Serve(ctx) }()
	t.Cleanup(func() { cancel(); d.Stop(); <-done })
	return d, names, roots
}

// writeTempFile 写一个随机内容的临时源文件,返回路径与内容。
func writeTempFile(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, data
}

// 端到端:dist_start → 轮询 dist_status 到结束 → 校验每台机器内容一致。
func TestDaemonDistributeEndToEnd(t *testing.T) {
	_, names, roots := startDistDaemon(t, 3)
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	ctx := context.Background()

	src, data := writeTempFile(t, 2<<20)
	resp, err := c.Do(ctx, protocol.CmdDistStart, protocol.DistStartParams{
		Sel:        protocol.HostSel{Group: "all"},
		LocalPath:  src,
		RemotePath: "recv/out.bin",
	})
	if err != nil {
		t.Fatalf("dist_start: %v", err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("dist_start 错误: %v", err)
	}
	var start protocol.DistStartResult
	if err := resp.Bind(&start); err != nil {
		t.Fatal(err)
	}
	if start.Total != 3 {
		t.Fatalf("目标应为 3, got %d", start.Total)
	}

	final := pollDistUntilDone(t, c, start.TaskID, 30*time.Second)
	if final.State != "done" || final.Done != 3 || final.Failed != 0 {
		t.Fatalf("最终状态不符: %+v", final)
	}

	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(roots[name], "recv", "out.bin"))
		if err != nil {
			t.Fatalf("读 %s 结果: %v", name, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("主机 %s 内容不一致", name)
		}
	}
	_ = names
}

// dist_status 对未知任务返回 404 语义错误。
func TestDaemonDistStatusUnknown(t *testing.T) {
	startDistDaemon(t, 1)
	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := c.Do(context.Background(), protocol.CmdDistStatus, protocol.DistStatusParams{TaskID: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Err() == nil {
		t.Fatal("未知任务应返回错误")
	}
}

// dist_start 缺参数应作为参数错误返回。
func TestDaemonDistStartBadParams(t *testing.T) {
	startDistDaemon(t, 1)
	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := c.Do(context.Background(), protocol.CmdDistStart, protocol.DistStartParams{
		Sel: protocol.HostSel{Group: "all"}, LocalPath: "", RemotePath: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Err() == nil {
		t.Fatal("缺路径参数应返回错误")
	}
}

// dist_cancel 应中止进行中的分发,且任务状态转为 canceled。
func TestDaemonDistCancel(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动服务器: %v", err)
	}
	t.Cleanup(srv.Close)
	srv.Root = t.TempDir()
	// 限速使分发足够慢,便于中途取消。
	srv.Throttle = &sshtest.Throttle{BytesPerSec: 512 << 10}

	cfgPath := filepath.Join(home, "hosts.yaml")
	yaml := fmt.Sprintf("hosts:\n  h1:\n    addr: %s\n    port: %d\n    user: %s\n    password: %s\ngroups:\n  all:\n    - h1\n",
		srv.Host(), srv.Port(), sshtest.User, sshtest.Password)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(Options{Addr: "127.0.0.1:0", ConfigPath: cfgPath, Version: "test", IdleTimeout: time.Hour})
	if err := d.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Serve(ctx) }()
	t.Cleanup(func() { cancel(); d.Stop(); <-done })

	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	src, _ := writeTempFile(t, 8<<20)
	resp, err := c.Do(context.Background(), protocol.CmdDistStart, protocol.DistStartParams{
		Sel: protocol.HostSel{Group: "all"}, LocalPath: src, RemotePath: "big.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	var start protocol.DistStartResult
	if err := resp.Bind(&start); err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond) // 让传输启动
	cresp, err := c.Do(context.Background(), protocol.CmdDistCancel, protocol.DistStatusParams{TaskID: start.TaskID})
	if err != nil {
		t.Fatalf("dist_cancel: %v", err)
	}
	if err := cresp.Err(); err != nil {
		t.Fatalf("dist_cancel 错误: %v", err)
	}

	final := pollDistUntilDone(t, c, start.TaskID, 10*time.Second)
	if final.State != "canceled" {
		t.Fatalf("取消后状态应为 canceled, got %s", final.State)
	}
}

// pollDistUntilDone 轮询 dist_status 直到任务不再是 running。
func pollDistUntilDone(t *testing.T, c *Client, taskID string, timeout time.Duration) protocol.DistStatusResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last protocol.DistStatusResult
	for time.Now().Before(deadline) {
		resp, err := c.Do(context.Background(), protocol.CmdDistStatus, protocol.DistStatusParams{TaskID: taskID})
		if err != nil {
			t.Fatalf("dist_status: %v", err)
		}
		if err := resp.Err(); err != nil {
			t.Fatalf("dist_status 错误: %v", err)
		}
		if err := resp.Bind(&last); err != nil {
			t.Fatal(err)
		}
		if last.State != "running" {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待分发完成超时,最后状态: %+v", last)
	return last
}
