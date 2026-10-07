package daemon

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/xelr233/sshfleet/internal/encoding"
	"github.com/xelr233/sshfleet/internal/protocol"
	"github.com/xelr233/sshfleet/internal/session"
	"github.com/xelr233/sshfleet/internal/sftp"
	"github.com/xelr233/sshfleet/internal/tail"
)

// ---- session ----

// sessionOpen 在主机上起一个持久 shell 会话,并把输出作为 EventOutput 推给本连接。
func (r *Router) sessionOpen(ctx context.Context, req *protocol.Request, conn Conn) (*protocol.Response, error) {
	var p protocol.SessionOpenParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	if p.Host == "" {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "session_open 需要 host"), nil
	}
	c, err := r.pool.Get(ctx, p.Host)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeUpstream, "%v", err), nil
	}
	sess, err := r.sessions.Open(r.baseCtx, c, p.Cols, p.Rows)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeUpstream, "%v", err), nil
	}

	id := sess.ID()
	// 会话输出 → EventOutput。即使发起 open 的 CLI 进程退出了,daemon 也必须继续
	// 接收输出(否则远端 shell 会因写满管道而阻塞),因此订阅一直保留到连接断开
	// 或会话关闭。订阅挂在连接上,连接断开时由 cancelSubs 统一取消。
	key := "session:" + id
	cancel := sess.Subscribe(func(b []byte) {
		_ = conn.Event(req.ID, protocol.EventOutput,
			protocol.SessionOutput{SessionID: id, Data: string(b)})
	})
	conn.AddSub(key, cancel)
	go func() {
		<-sess.Ended()
		conn.DelSub(key)
		_ = conn.Event(req.ID, protocol.EventSessionEnd, protocol.SessionOutput{SessionID: id})
	}()

	return protocol.OK(req.ID, protocol.SessionOpenResult{SessionID: id})
}

// sessionSend 向会话写入输入,并回传随之产生的输出。
//
// 交互式 shell 没有「这次输出到此结束」的信号,因此以「静默」为界:收到输出后
// 持续 250ms 无新输出即认为本次响应结束,最长等待 sendTimeout。
func (r *Router) sessionSend(req *protocol.Request, conn Conn) (*protocol.Response, error) {
	var p protocol.SessionSendParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	sess, ok := r.sessions.Get(p.SessionID)
	if !ok {
		return protocol.Fail(req.ID, protocol.CodeNotFound, "会话 %q 不存在", p.SessionID), nil
	}

	data := captureOutput(sess, func() error { return sess.Write(p.Input) })
	return protocol.OK(req.ID, protocol.SessionOutput{SessionID: sess.ID(), Data: data})
}

func (r *Router) sessionClose(req *protocol.Request, conn Conn) (*protocol.Response, error) {
	var p protocol.SessionCloseParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	if err := r.sessions.Close(p.SessionID); err != nil {
		return protocol.Fail(req.ID, protocol.CodeNotFound, "%v", err), nil
	}
	if conn != nil {
		conn.DelSub("session:" + p.SessionID)
	}
	return protocol.OK(req.ID, map[string]string{"session_id": p.SessionID, "state": "closed"})
}

func (r *Router) sessionList() *protocol.SessionListResult {
	infos := r.sessions.List()
	out := make([]protocol.SessionInfo, 0, len(infos))
	for _, in := range infos {
		out = append(out, protocol.SessionInfo{
			SessionID: in.ID, Host: in.Host, OpenedAt: in.OpenedAt,
			Idle: in.Idle.Round(time.Second).String(),
		})
	}
	return &protocol.SessionListResult{Sessions: out}
}

// captureOutput 在写入输入的前后订阅会话输出,直到静默或超时,返回捕获的字节。
func captureOutput(sess *session.Session, write func() error) string {
	var mu sync.Mutex
	var buf []byte
	last := time.Now()
	done := make(chan struct{})

	cancel := sess.Subscribe(func(b []byte) {
		mu.Lock()
		buf = append(buf, b...)
		last = time.Now()
		mu.Unlock()
	})
	defer cancel()

	if err := write(); err != nil {
		return ""
	}

	// 空闲(距最近输出)达到 idle 即认为本次输出了结;硬上限 hardLimit。
	const idle = 250 * time.Millisecond
	const hardLimit = 3 * time.Second
	deadline := time.Now().Add(hardLimit)
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			mu.Lock()
			got, quiet := len(buf) > 0, time.Since(last) >= idle
			mu.Unlock()
			if got && quiet {
				close(done)
				return
			}
			if time.Now().After(deadline) {
				close(done)
				return
			}
		}
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	return encoding.ToUTF8(buf)
}

// ---- tail ----

// tailStart 启动或重连一个 tail 任务,并把行作为 EventTailLine 推给本连接。
//
// 重连:传 TaskID + Since(已收到的最大 seq),从环形缓冲回放缺失的行后再续推,
// 因此客户端断开重连不丢已推事件(README §9 M4 验收)。
func (r *Router) tailStart(ctx context.Context, req *protocol.Request, conn Conn) (*protocol.Response, error) {
	var p protocol.TailStartParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}

	var t *tail.Task
	if p.TaskID != "" {
		existing, ok := r.tails.Get(p.TaskID)
		if !ok {
			return protocol.Fail(req.ID, protocol.CodeNotFound, "tail 任务 %q 不存在", p.TaskID), nil
		}
		t = existing
	} else {
		if p.Host == "" || p.Path == "" {
			return protocol.Fail(req.ID, protocol.CodeBadRequest, "tail_start 需要 host 与 path"), nil
		}
		nt, err := r.tails.Start(r.pool, p.Host, p.Path)
		if err != nil {
			return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
		}
		t = nt
	}

	id := t.ID()
	key := "tail:" + id
	cancel := t.Subscribe(p.Since, func(ln tail.Line) {
		_ = conn.Event(req.ID, protocol.EventTailLine,
			protocol.TailLine{TaskID: id, Seq: ln.Seq, Line: ln.Text})
	})
	conn.AddSub(key, cancel)
	go func() {
		<-t.Done()
		conn.DelSub(key)
	}()

	return protocol.OK(req.ID, protocol.TailStartResult{TaskID: id})
}

// tailStop 停止 tail 任务。
func (r *Router) tailStop(req *protocol.Request, conn Conn) (*protocol.Response, error) {
	var p protocol.TailStopParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	if err := r.tails.Stop(p.TaskID); err != nil {
		return protocol.Fail(req.ID, protocol.CodeNotFound, "%v", err), nil
	}
	if conn != nil {
		conn.DelSub("tail:" + p.TaskID)
	}
	return protocol.OK(req.ID, map[string]string{"task_id": p.TaskID, "state": "stopped"})
}

// ---- sftp (M6) ----

// sftpTransfer 处理 sftp_put / sftp_get:单机传输,进度作为 EventProgress 推送。
func (r *Router) sftpTransfer(ctx context.Context, req *protocol.Request, conn Conn, download bool) (*protocol.Response, error) {
	var p protocol.SFTPParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	if p.Host == "" || p.Local == "" || p.Remote == "" {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "sftp 需要 host / local / remote"), nil
	}

	c, err := r.pool.Get(ctx, p.Host)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeUpstream, "%v", err), nil
	}
	cli, err := sftp.Open(ctx, c)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeUpstream, "%v", err), nil
	}
	defer cli.Close()

	onProgress := func(off, total int64) {
		if conn != nil {
			_ = conn.Event(req.ID, protocol.EventProgress,
				protocol.SFTPProgress{Host: p.Host, Bytes: off, Total: total})
		}
	}

	start := time.Now()
	var n int64
	if download {
		n, err = sftpGetOne(ctx, cli, p, onProgress)
	} else {
		n, err = sftpPutOne(ctx, cli, p, onProgress)
	}
	res := protocol.SFTPResult{Host: p.Host, Bytes: n, Duration: time.Since(start).String()}
	if err != nil {
		res.Err = err.Error()
	}
	return protocol.OK(req.ID, res)
}

// sftpPutOne 打开本地文件写入远端(pool 里已开户的 SFTP 会话)。
func sftpPutOne(ctx context.Context, cli *sftp.Client, p protocol.SFTPParams, onProgress func(int64, int64)) (int64, error) {
	f, err := os.Open(p.Local)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	total := int64(0)
	if fi, err := f.Stat(); err == nil {
		total = fi.Size()
	}
	return cli.Put(ctx, f, p.Remote, sftp.PutOptions{Total: total, Mkdir: true, OnProgress: onProgress})
}

// sftpGetOne 把远端文件读入本地。
func sftpGetOne(ctx context.Context, cli *sftp.Client, p protocol.SFTPParams, onProgress func(int64, int64)) (int64, error) {
	dst, err := os.OpenFile(p.Local, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	return cli.Get(ctx, dst, p.Remote, sftp.PutOptions{OnProgress: onProgress})
}

// ---- hosts test (M6) ----

// hostsTest 对选中的主机逐一建连(含认证),报告是否成功与耗时。
// 复用连接池:已连的主机近乎瞬时返回,未连的触发一次真实握手。
func (r *Router) hostsTest(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	var p protocol.HostsTestParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	names, err := r.resolve(p.Sel)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}

	out := protocol.HostsTestResult{Hosts: make([]protocol.HostTestState, 0, len(names))}
	for _, name := range names {
		st := protocol.HostTestState{Host: name}
		t0 := time.Now()
		if _, err := r.pool.Get(ctx, name); err != nil {
			st.Err = err.Error()
		} else {
			st.OK = true
			st.Latency = time.Since(t0).Round(time.Millisecond).String()
		}
		out.Hosts = append(out.Hosts, st)
	}
	return protocol.OK(req.ID, out)
}
