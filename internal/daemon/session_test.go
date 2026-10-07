package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/protocol"
)

// M4 端到端:session_open → session_send(cd) → session_send(pwd) → close/list。
// 用同一连接(会话输出事件推给打开它的连接)。
func TestDaemonSessionEndToEnd(t *testing.T) {
	startTestDaemon(t)
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	ctx := context.Background()

	// open
	resp, err := c.Do(ctx, protocol.CmdSessionOpen, protocol.SessionOpenParams{Host: "h1"})
	if err != nil {
		t.Fatalf("session_open: %v", err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("session_open 错误: %v", err)
	}
	var openRes protocol.SessionOpenResult
	if err := resp.Bind(&openRes); err != nil {
		t.Fatal(err)
	}
	id := openRes.SessionID
	if id == "" {
		t.Fatal("session_open 应返回 session_id")
	}

	// list 应含该会话
	resp, err = c.Do(ctx, protocol.CmdSessionList, nil)
	if err != nil {
		t.Fatalf("session_list: %v", err)
	}
	var list protocol.SessionListResult
	if err := resp.Bind(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != id {
		t.Fatalf("session_list 不符: %+v", list.Sessions)
	}

	// cd /tmp
	resp, err = c.Do(ctx, protocol.CmdSessionSend, protocol.SessionSendParams{SessionID: id, Input: "cd /tmp\n"})
	if err != nil {
		t.Fatalf("session_send cd: %v", err)
	}
	var out1 protocol.SessionOutput
	if err := resp.Bind(&out1); err != nil {
		t.Fatal(err)
	}
	if !contains(out1.Data, "cd ok -> /tmp") {
		t.Fatalf("cd 输出不符: %q", out1.Data)
	}

	// pwd 反映 cd(状态延续)
	resp, err = c.Do(ctx, protocol.CmdSessionSend, protocol.SessionSendParams{SessionID: id, Input: "pwd\n"})
	if err != nil {
		t.Fatalf("session_send pwd: %v", err)
	}
	var out2 protocol.SessionOutput
	if err := resp.Bind(&out2); err != nil {
		t.Fatal(err)
	}
	if !contains(out2.Data, "/tmp") {
		t.Fatalf("pwd 应输出 /tmp, got %q", out2.Data)
	}

	// close
	resp, err = c.Do(ctx, protocol.CmdSessionClose, protocol.SessionCloseParams{SessionID: id})
	if err != nil {
		t.Fatalf("session_close: %v", err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("session_close 错误: %v", err)
	}
}

// session_send / close 对未知会话返回错误。
func TestDaemonSessionUnknown(t *testing.T) {
	startTestDaemon(t)
	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Do(context.Background(), protocol.CmdSessionSend, protocol.SessionSendParams{SessionID: "sess-999", Input: "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Err() == nil {
		t.Fatal("未知会话应返回错误")
	}
}

// M4 端到端:tail_start 持续收到 EventTailLine;带 seq 重连补齐缺口。
func TestDaemonTailStreamAndReconnect(t *testing.T) {
	d := startTestDaemon(t)
	_ = d
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 首个客户端:开 tail,收若干行后断开连接(模拟断线)。
	c1, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var mu sync.Mutex
	var first []protocol.TailLine
	var taskID string
	got3 := make(chan struct{})
	streamCtx, streamCancel := context.WithCancel(ctx)
	go func() {
		_ = c1.Stream(streamCtx, protocol.CmdTailStart, protocol.TailStartParams{Host: "h1", Path: "/x"},
			func(ev *protocol.Event) {
				if ev.Event != protocol.EventTailLine {
					return
				}
				var ln protocol.TailLine
				if err := ev.Bind(&ln); err != nil {
					return
				}
				mu.Lock()
				first = append(first, ln)
				n := len(first)
				mu.Unlock()
				if n == 3 {
					close(got3)
				}
			},
			func(resp *protocol.Response) {
				if resp.Err() != nil {
					return
				}
				var r protocol.TailStartResult
				_ = resp.Bind(&r)
				mu.Lock()
				taskID = r.TaskID
				mu.Unlock()
			})
	}()

	select {
	case <-got3:
	case <-time.After(5 * time.Second):
		t.Fatal("首个客户端未收到 3 行 tail")
	}
	// 断开首个客户端:其订阅应被自动取消(向死连接不再推送)。
	streamCancel()
	c1.Close()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	last := first[len(first)-1].Seq
	id := taskID
	mu.Unlock()
	if id == "" {
		t.Fatal("未拿到 task_id")
	}

	// 重连:新客户端带 since=last,应从环形缓冲补齐缺口(不丢事件)。
	c2, err := Dial()
	if err != nil {
		t.Fatalf("重连 Dial: %v", err)
	}
	defer c2.Close()

	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	var replay []protocol.TailLine
	rgot := make(chan struct{})
	go func() {
		_ = c2.Stream(rctx, protocol.CmdTailStart, protocol.TailStartParams{TaskID: id, Since: last},
			func(ev *protocol.Event) {
				if ev.Event != protocol.EventTailLine {
					return
				}
				var ln protocol.TailLine
				if err := ev.Bind(&ln); err != nil {
					return
				}
				mu.Lock()
				replay = append(replay, ln)
				n := len(replay)
				mu.Unlock()
				if n == 3 {
					close(rgot)
				}
			}, nil)
	}()
	select {
	case <-rgot:
	case <-time.After(5 * time.Second):
		t.Fatal("重连客户端未补齐 3 行")
	}

	mu.Lock()
	defer mu.Unlock()
	want := last
	for i, ln := range replay {
		want++
		if ln.Seq != want {
			t.Fatalf("重连第 %d 行 seq 应为 %d, got %d(丢/重事件)", i, want, ln.Seq)
		}
	}
}

// sftp_put 端到端:写入远端(测试服务器的 SFTP 根为临时目录)。
func TestDaemonSFTPPutGet(t *testing.T) {
	d, _, roots := startDistDaemon(t, 1)
	_ = d
	root := roots["a"]

	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	// 本地源文件
	src, data := writeTempFile(t, 1<<20)

	var progress int64
	resp, err := c.DoEvents(ctx, protocol.CmdSFTPPut, protocol.SFTPParams{
		Host: "a", Local: src, Remote: "up/out.bin",
	}, func(ev *protocol.Event) {
		if ev.Event == protocol.EventProgress {
			var p protocol.SFTPProgress
			if ev.Bind(&p) == nil {
				progress = p.Bytes
			}
		}
	})
	if err != nil {
		t.Fatalf("sftp_put: %v", err)
	}
	var res protocol.SFTPResult
	if err := resp.Bind(&res); err != nil {
		t.Fatal(err)
	}
	if res.Err != "" {
		t.Fatalf("sftp_put 错误: %s", res.Err)
	}
	if res.Bytes != int64(len(data)) {
		t.Fatalf("上传字节数应为 %d, got %d", len(data), res.Bytes)
	}
	if progress == 0 {
		t.Fatal("应收到进度事件")
	}

	// 校验落盘内容
	got, err := os.ReadFile(filepath.Join(root, "up", "out.bin"))
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("上传内容不一致")
	}

	// 下载回环
	dst := src + ".back"
	resp, err = c.Do(ctx, protocol.CmdSFTPGet, protocol.SFTPParams{Host: "a", Remote: "up/out.bin", Local: dst})
	if err != nil {
		t.Fatalf("sftp_get: %v", err)
	}
	if err := resp.Bind(&res); err != nil {
		t.Fatal(err)
	}
	if res.Err != "" {
		t.Fatalf("sftp_get 错误: %s", res.Err)
	}
	back, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Fatal("下载内容不一致")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
