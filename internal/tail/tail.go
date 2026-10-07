// Package tail 实现远端文件的流式跟随(类似 tail -f)。
//
// 每个 tail 任务在远端跑 `tail -n 0 -f`,把新行读入一个**环形缓冲**并按序编号,
// 再分发给订阅者。订阅者带一个游标(seq):断线重连时以 `since=<上次收到的 seq>`
// 重新订阅,即可从环形缓冲补回断线期间的行 —— 这就是 README §9 M4 的
// 「客户端断开重连不丢已推事件(环形缓冲)」。
package tail

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/xelr233/sshfleet/internal/encoding"
	"github.com/xelr233/sshfleet/internal/pool"
)

// DefaultBufferLines 是环形缓冲保留的最大行数。超出后丢弃最旧的行,
// 因此重连只能补回最近这么多行(缓冲区之外的历史无法恢复)。
const DefaultBufferLines = 2000

// maxLineBytes 限制单行长度(与 bufio.Scanner 的缓冲上限一致)。
const maxLineBytes = 1 << 20

// Line 是一行输出及其全局递增序号(从 1 开始)。
type Line struct {
	Seq  int64
	Text string
}

// Info 是 tail 任务的对外快照。
type Info struct {
	ID     string
	Host   string
	Path   string
	Seq    int64
	Closed bool
	Err    string
}

// Task 是一个 tail 任务。自包含:Progress 快照、Subscribe 订阅、Stop 停止。
type Task struct {
	id   string
	host string
	path string

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	lines   []Line // 环形缓冲(保留最近 DefaultBufferLines 行)
	nextSeq int64
	subs    map[int]*sub
	nextSub int
	closed  bool
	errMsg  string

	done chan struct{}
}

type sub struct {
	since int64
	fn    func(Line)
}

// ID 返回任务 id。
func (t *Task) ID() string { return t.id }

// Info 返回任务快照。
func (t *Task) Info() Info {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Info{ID: t.id, Host: t.host, Path: t.path, Seq: t.nextSeq, Closed: t.closed, Err: t.errMsg}
}

// Done 在任务结束时关闭。
func (t *Task) Done() <-chan struct{} { return t.done }

// Subscribe 订阅行事件(since 为已收到的最大 seq,0 表示从头回放缓冲)。返回取消函数。
//
// 先回放缓冲区中 seq > since 的行,再转为实时分发,因此不会丢也不会重
// (在环形缓冲覆盖范围内)。
func (t *Task) Subscribe(since int64, fn func(Line)) func() {
	t.mu.Lock()
	for _, ln := range t.lines {
		if ln.Seq > since {
			fn(ln)
		}
	}
	if t.nextSeq > since {
		since = t.nextSeq
	}
	if t.subs == nil {
		t.subs = map[int]*sub{}
	}
	id := t.nextSub
	t.nextSub++
	t.subs[id] = &sub{since: since, fn: fn}
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		delete(t.subs, id)
		t.mu.Unlock()
	}
}

// Stop 停止任务(幂等)。
func (t *Task) Stop() { t.cancel() }

// append 追加一行:编号、进环形缓冲、分发给游标落后的订阅者。
func (t *Task) append(text string) {
	text = encoding.ToUTF8([]byte(text)) // GBK 兜底解码(README §5.9)
	t.mu.Lock()
	t.nextSeq++
	ln := Line{Seq: t.nextSeq, Text: text}
	t.lines = append(t.lines, ln)
	if len(t.lines) > DefaultBufferLines {
		t.lines = t.lines[len(t.lines)-DefaultBufferLines:]
	}
	type delivery struct {
		fn func(Line)
		ln Line
	}
	var out []delivery
	for _, sb := range t.subs {
		if ln.Seq > sb.since {
			sb.since = ln.Seq
			out = append(out, delivery{fn: sb.fn, ln: ln})
		}
	}
	t.mu.Unlock()
	for _, d := range out {
		d.fn(d.ln)
	}
}

func (t *Task) setErr(err error) {
	t.mu.Lock()
	if !t.closed {
		t.errMsg = err.Error()
	}
	t.mu.Unlock()
}

// run 起远端 `tail -n 0 -f` 并把输出行送入环形缓冲。
func (t *Task) run(p *pool.Pool) {
	defer close(t.done)
	defer func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
	}()

	conn, err := p.Get(t.ctx, t.host)
	if err != nil {
		t.setErr(fmt.Errorf("取连接: %w", err))
		return
	}
	if err := conn.Acquire(t.ctx); err != nil {
		t.setErr(err)
		return
	}
	defer conn.Release()

	sess, err := conn.Client().NewSession()
	if err != nil {
		t.setErr(fmt.Errorf("建立会话: %w", err))
		return
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.setErr(fmt.Errorf("取 stdout: %w", err))
		return
	}
	// ctx 取消时关会话,解除阻塞中的 Scan。
	go func() {
		select {
		case <-t.ctx.Done():
			sess.Close()
		case <-t.done:
		}
	}()

	if err := sess.Start("tail -n 0 -f -- " + shellQuote(t.path)); err != nil {
		t.setErr(fmt.Errorf("启动 tail: %w", err))
		return
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		t.append(sc.Text())
	}
	if err := sc.Err(); err != nil && t.ctx.Err() == nil {
		t.setErr(err)
	}
}

// Manager 持有所有 tail 任务。并发安全。
type Manager struct {
	mu    sync.Mutex
	tasks map[string]*Task
	seq   int
}

// NewManager 构造空的 tail 管理器。
func NewManager() *Manager {
	return &Manager{tasks: map[string]*Task{}}
}

// Start 新建一个 tail 任务并异步运行。pool 用于取连接。
func (m *Manager) Start(p *pool.Pool, host, path string) (*Task, error) {
	if host == "" || path == "" {
		return nil, fmt.Errorf("tail: 需要 host 与 path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("tail-%d", m.seq)
	t := &Task{
		id: id, host: host, path: path,
		ctx: ctx, cancel: cancel,
		done: make(chan struct{}),
	}
	m.tasks[id] = t
	m.mu.Unlock()

	go t.run(p)
	return t, nil
}

// Get 按 id 取任务。
func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok
}

// List 返回所有任务快照,按 id 排序。
func (m *Manager) List() []Info {
	m.mu.Lock()
	out := make([]Info, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t.Info())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stop 停止并移除指定任务。不存在时返回错误。
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	delete(m.tasks, id)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("tail: 任务 %q 不存在", id)
	}
	t.Stop()
	return nil
}

// StopAll 停止所有任务(daemon 退出时调用)。
func (m *Manager) StopAll() {
	m.mu.Lock()
	all := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		all = append(all, t)
	}
	m.tasks = map[string]*Task{}
	m.mu.Unlock()
	for _, t := range all {
		t.Stop()
	}
	// 等待所有任务真正结束,避免 run goroutine 在 daemon 关闭后仍持有连接。
	for _, t := range all {
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
		}
	}
}

// shellQuote 用单引号安全包裹一个 POSIX shell 参数(路径可能含空格/引号)。
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '"', '\'', '"', '\'') // 结束单引号,插入 '"'"',重开
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}
