// Package session 实现 SSH shell 会话(持久 stdin/stdout)。
//
// 与 exec channel(一次执行、无状态)不同,会话用 ssh.Session.Shell() 起一个
// 交互式登录 shell 并把 stdin/stdout 持久连到它,因此 cd、环境变量等状态在
// 多次 send 之间延续(README §5.6)。
//
// 会话由 daemon 持有:CLI 是无状态薄客户端,`session open` 之后进程即退出,
// 会话必须活在 daemon 里,后续 `session send <id>`(新连接)才能命中同一会话。
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/xelr233/sshfleet/internal/pool"
)

// closeGrace 是 Close 等待会话真正结束的上限。
const closeGrace = 2 * time.Second

// Info 是会话的对外快照(供 session_list 展示)。
type Info struct {
	ID       string
	Host     string
	OpenedAt time.Time
	Idle     time.Duration
}

// Session 是一个活动的远端 shell 会话。
//
// 输出由后台 goroutine 持续读取并通过 Subscribe 注册的回调分发;会话本身
// 不保留输出历史(需要回放历史的是 tail,见 internal/tail)。
type Session struct {
	id       string
	host     string
	openedAt time.Time

	sess      *ssh.Session
	conn      *pool.Conn
	stdinPipe io.WriteCloser

	mu         sync.Mutex
	subs       map[int]func([]byte)
	nextSub    int
	lastActive time.Time
	closed     bool

	endOnce     sync.Once
	releaseOnce sync.Once // 保证 channel 名额只归还一次
	ended       chan struct{}
}

// ID 返回会话 id。
func (s *Session) ID() string { return s.id }

// Host 返回会话所在主机名。
func (s *Session) Host() string { return s.host }

// Info 返回会话快照。
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{ID: s.id, Host: s.host, OpenedAt: s.openedAt, Idle: time.Since(s.lastActive)}
}

// Closed 报告会话是否已结束(远端退出、EOF 或显式关闭)。
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Ended 在会话结束时被关闭,供上层清理时等待。
func (s *Session) Ended() <-chan struct{} { return s.ended }

// Write 把 input 写进会话 stdin(通常以换行结尾,由调用方决定)。
func (s *Session) Write(input string) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("session: 会话 %s 已结束", s.id)
	}
	if _, err := io.WriteString(s.stdinPipe, input); err != nil {
		return fmt.Errorf("session: 写入会话 %s: %w", s.id, err)
	}
	s.touch()
	return nil
}

// Subscribe 注册一个输出回调,返回取消订阅函数。
// 回调在会话的读 goroutine 中串行调用;实现应尽快返回,避免拖慢读取。
func (s *Session) Subscribe(fn func([]byte)) func() {
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[int]func([]byte){}
	}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

// Close 关闭会话(可重复调用),关闭后其 channel 名额被归还。
//
// 先关闭 stdin(向远端 shell 发 EOF),再关会话通道。等待会话 goroutine 收尾有
// 上限:某些 sshd(如 Windows OpenSSH 的 PTY shell)关闭通道后不一定回送
// exit-status,若无限等待会把 daemon 请求挂死。超时后主动标记结束并释放名额,
// 后台 goroutine 在真正结束时再做幂等清理。
func (s *Session) Close() error {
	if s.Closed() {
		select {
		case <-s.ended:
		case <-time.After(closeGrace):
		}
		return nil
	}
	_ = s.stdinPipe.Close()
	err := s.sess.Close()
	// 关 stdin 后远端 shell 可能已先行退出并关闭通道,某些平台(如 Linux 的
	// 原生 sshd)此时 sess.Close() 会回一个 EOF —— 会话仍处于拆除中,这不是
	// 失败,不应向上层报错。
	if err != nil && errors.Is(err, io.EOF) {
		err = nil
	}
	select {
	case <-s.ended:
	case <-time.After(closeGrace):
		s.end() // 标记结束,唤醒等待者
	}
	s.release()
	return err
}

// release 归还 channel 名额(只执行一次;会话 goroutine 与 Close 超时路径都会调)。
func (s *Session) release() {
	s.releaseOnce.Do(func() { s.conn.Release() })
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// emit 把一段输出分发给所有订阅者。
func (s *Session) emit(p []byte) {
	s.mu.Lock()
	s.lastActive = time.Now()
	subs := make([]func([]byte), 0, len(s.subs))
	for _, fn := range s.subs {
		subs = append(subs, fn)
	}
	s.mu.Unlock()
	for _, fn := range subs {
		fn(p)
	}
}

// end 标记会话结束并唤醒等待者(只执行一次)。
func (s *Session) end() {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.ended)
	})
}

// Manager 持有所有活动会话。并发安全。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	seq      int
}

// NewManager 构造空的会话管理器。
func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// Open 在 conn 上建立一个 shell 会话。会占用一个 channel 名额直到会话结束。
//
// 会先请求一个伪终端(PTY):Windows OpenSSH 对非 PTY 的 shell 请求会直接断开
// 连接,必须请求 PTY 才起得来交互式 shell。PTY 请求失败时不中断(部分精简
// sshd 不支持),继续尝试直接用 shell。
func (m *Manager) Open(ctx context.Context, conn *pool.Conn, cols, rows int) (*Session, error) {
	if conn == nil {
		return nil, fmt.Errorf("session: 连接为空")
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if err := conn.Acquire(ctx); err != nil {
		return nil, err
	}
	sess, err := conn.Client().NewSession()
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("session: 建立会话: %w", err)
	}
	// PTY 是交互式 shell 的前提(Windows sshd 尤其)。失败仅记录,不阻断。
	_ = sess.RequestPty("xterm", rows, cols, ssh.TerminalModes{})

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		conn.Release()
		return nil, fmt.Errorf("session: 取 stdin: %w", err)
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("sess-%d", m.seq)
	s := &Session{
		id:        id,
		host:      conn.Host().Name,
		openedAt:  time.Now(),
		sess:      sess,
		conn:      conn,
		stdinPipe: stdin,
		ended:     make(chan struct{}),
	}
	s.lastActive = s.openedAt
	m.sessions[id] = s
	m.mu.Unlock()

	// 把 stdout/stderr 都汇入 emit,再启动登录 shell。
	sess.Stdout = sessionWriter{s}
	sess.Stderr = sessionWriter{s}
	if err := sess.Shell(); err != nil {
		sess.Close()
		conn.Release()
		m.remove(id)
		return nil, fmt.Errorf("session: 启动 shell: %w", err)
	}

	// 会话生命周期:等待远端结束 → 标记结束、归还 channel 名额、从表中移除。
	go func() {
		_ = sess.Wait()
		s.end()
		s.release()
		m.remove(id)
	}()
	return s, nil
}

// Get 按 id 取会话。
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List 返回所有活动会话快照,按 id 排序。
func (m *Manager) List() []Info {
	m.mu.Lock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Info())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close 关闭指定会话。不存在时返回错误。
// 关闭后立即从表中移除,不依赖会话 goroutine 的收尾(部分 sshd 的 Wait 不回)。
func (m *Manager) Close(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session: 会话 %q 不存在", id)
	}
	err := s.Close()
	m.remove(id)
	return err
}

// CloseAll 关闭所有会话(daemon 退出时调用)。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		_ = s.Close()
	}
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// sessionWriter 把写入的字节转发给会话的 emit。
type sessionWriter struct{ s *Session }

func (w sessionWriter) Write(p []byte) (int, error) {
	w.s.emit(p)
	return len(p), nil
}
