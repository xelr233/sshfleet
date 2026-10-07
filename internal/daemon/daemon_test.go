package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/protocol"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// startTestDaemon 在隔离的 SSHFLEET_HOME 下启动一个 daemon,并写入指向测试
// SSH 服务器(主机名 h1)的 hosts.yaml。返回 daemon 与清理函数。
func startTestDaemon(t *testing.T) *Daemon {
	t.Helper()

	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动测试 SSH 服务器: %v", err)
	}
	t.Cleanup(srv.Close)

	cfgPath := filepath.Join(home, "hosts.yaml")
	yaml := fmt.Sprintf("hosts:\n  h1:\n    addr: %s\n    port: %d\n    user: %s\n    password: %s\ngroups:\n  all:\n    - h1\n",
		srv.Host(), srv.Port(), sshtest.User, sshtest.Password)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("写 hosts.yaml: %v", err)
	}

	d := New(Options{Addr: "127.0.0.1:0", ConfigPath: cfgPath, Version: "test", IdleTimeout: time.Hour})
	if err := d.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		d.Stop()
		<-done
	})
	return d
}

func TestDaemonStatusAndExec(t *testing.T) {
	startTestDaemon(t)

	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// status
	resp, err := c.Do(ctx, protocol.CmdStatus, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("status 返回错误: %v", err)
	}
	var st protocol.StatusResult
	if err := resp.Bind(&st); err != nil {
		t.Fatalf("解析 status: %v", err)
	}
	if st.Version != "test" {
		t.Fatalf("版本应为 test, got %q", st.Version)
	}

	// exec
	resp, err = c.Do(ctx, protocol.CmdExec, protocol.ExecParams{Host: "h1", Command: "ping"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("exec 返回错误: %v", err)
	}
	var er protocol.ExecResult
	if err := resp.Bind(&er); err != nil {
		t.Fatalf("解析 exec: %v", err)
	}
	if er.Stdout != "echo:ping\n" {
		t.Fatalf("exec stdout 不符: %q", er.Stdout)
	}
	if er.Err != "" {
		t.Fatalf("exec 不应有连接层错误: %q", er.Err)
	}
}

// 同一连接上的多个请求必须按 id 正确关联(并发请求不错配)。
func TestDaemonConcurrentRequestsOnOneConnection(t *testing.T) {
	startTestDaemon(t)
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// 用不同的命令逐个请求,校验各自的响应对应正确。
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("cmd%d", i)
		resp, err := c.Do(context.Background(), protocol.CmdExec, protocol.ExecParams{Host: "h1", Command: want})
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		var er protocol.ExecResult
		if err := resp.Bind(&er); err != nil {
			t.Fatalf("解析 %d: %v", i, err)
		}
		if er.Stdout != "echo:"+want+"\n" {
			t.Fatalf("第 %d 条响应错配: got %q want %q", i, er.Stdout, "echo:"+want)
		}
	}
}

// 错误 token 必须被拒绝:服务端不响应任何请求即断开。
func TestDaemonRejectsBadToken(t *testing.T) {
	d := startTestDaemon(t)

	port := d.Addr().(*net.TCPAddr).Port
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("wrong-token\n")); err != nil {
		t.Fatalf("写 token: %v", err)
	}
	// 发送一个合法请求,服务端应因 token 不匹配直接断开,读到的应是 EOF。
	if _, err := conn.Write([]byte(`{"id":1,"cmd":"status"}` + "\n")); err != nil {
		t.Fatalf("写请求: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("错误 token 不应收到响应, got %q", buf[:n])
	}
}

// 多个客户端可同时连接(daemon 支持多连接)。
func TestDaemonMultipleClients(t *testing.T) {
	startTestDaemon(t)
	a, err := Dial()
	if err != nil {
		t.Fatalf("客户端 A: %v", err)
	}
	defer a.Close()
	b, err := Dial()
	if err != nil {
		t.Fatalf("客户端 B: %v", err)
	}
	defer b.Close()

	for _, c := range []*Client{a, b} {
		resp, err := c.Do(context.Background(), protocol.CmdStatus, nil)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if resp.Err() != nil {
			t.Fatalf("status 错误: %v", resp.Err())
		}
	}
}

// 未知命令应作为协议错误返回,而不是让连接静默失败。
func TestDaemonUnknownCommand(t *testing.T) {
	startTestDaemon(t)
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	resp, err := c.Do(context.Background(), protocol.CmdBatch, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Err() == nil {
		t.Fatal("未实现的命令应返回错误响应")
	}
}

// batch 走 group 解析 + 并发执行:逐机进度事件先到,汇总响应后到。
func TestDaemonBatchGroupWithEvents(t *testing.T) {
	startTestDaemon(t)
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var events []protocol.ExecResult
	var mu sync.Mutex
	resp, err := c.DoEvents(context.Background(), protocol.CmdBatch, protocol.BatchParams{
		Sel:     protocol.HostSel{Group: "all"},
		Command: "ping",
	}, func(ev *protocol.Event) {
		if ev.Event == protocol.EventProgress {
			var er protocol.ExecResult
			if err := ev.Bind(&er); err == nil {
				mu.Lock()
				events = append(events, er)
				mu.Unlock()
			}
		}
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("batch 返回错误: %v", err)
	}
	var res protocol.BatchResult
	if err := resp.Bind(&res); err != nil {
		t.Fatalf("解析 batch: %v", err)
	}
	if res.Total != 1 || res.OK != 1 || res.Failed != 0 {
		t.Fatalf("汇总不符: %+v", res)
	}
	if len(res.Results) != 1 || res.Results[0].Stdout != "echo:ping\n" {
		t.Fatalf("逐机结果不符: %+v", res.Results)
	}
	if len(events) == 0 {
		t.Fatal("应至少收到一条逐机进度事件")
	}
}

// batch 的未知分组应作为参数错误返回。
func TestDaemonBatchUnknownGroup(t *testing.T) {
	startTestDaemon(t)
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	resp, err := c.Do(context.Background(), protocol.CmdBatch, protocol.BatchParams{
		Sel:     protocol.HostSel{Group: "nope"},
		Command: "hi",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Err() == nil {
		t.Fatal("未知分组应返回错误")
	}
}
