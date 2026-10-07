package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/protocol"
)

// Options 是 daemon 的配置。
type Options struct {
	// Addr 是监听地址,默认 127.0.0.1:0(随机端口,README §5.1)。
	Addr string
	// ConfigPath 是 hosts.yaml 路径;空则用默认路径。
	ConfigPath string
	// IdleTimeout 是无客户端、无活动任务时的自杀超时;0 表示不退出。
	IdleTimeout time.Duration
	// Version 由 CLI 注入,用于 status 命令。
	Version string
	// Logf 是日志输出;nil 时丢弃。
	Logf func(format string, args ...any)
}

func (o Options) withDefaults() Options {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:0"
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 30 * time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// Daemon 持有监听套接字、连接池与所有活动客户端。
type Daemon struct {
	opts  Options
	token string

	ln     net.Listener
	router *Router

	mu      sync.Mutex
	clients map[*clientConn]struct{}
	// activeTasks 是活动任务计数(distribute / batch 等长任务),空闲判定需一并考虑。
	activeTasks atomic.Int64

	lastActivity atomic.Int64 // UnixNano,无活动时用于空闲退出

	quit     chan struct{}
	quitOnce sync.Once
	wg       sync.WaitGroup
}

// New 构造 daemon(不开始监听)。
func New(opts Options) *Daemon {
	opts = opts.withDefaults()
	d := &Daemon{
		opts:    opts,
		clients: map[*clientConn]struct{}{},
		quit:    make(chan struct{}),
	}
	d.touch()
	return d
}

// Listen 绑定端口、生成 token 并落盘运行状态,随后返回(不阻塞)。
// 调用方接着调用 Serve。
func (d *Daemon) Listen() error {
	token, err := newToken()
	if err != nil {
		return err
	}
	d.token = token

	ln, err := net.Listen("tcp", d.opts.Addr)
	if err != nil {
		return fmt.Errorf("daemon: 监听 %s: %w", d.opts.Addr, err)
	}
	d.ln = ln

	port := ln.Addr().(*net.TCPAddr).Port

	// 连接池只在 daemon 进程内存在,是「一次握手、多次复用」的载体。
	p, cfg := newPool(d.opts.ConfigPath, d.opts.Logf)
	d.router = NewRouter(p, cfg, d.opts.Version)
	d.router.onTaskStart = func() { d.activeTasks.Add(1) }
	d.router.onTaskEnd = func() { d.activeTasks.Add(-1) }

	if err := config.EnsureDir(); err != nil {
		ln.Close()
		return fmt.Errorf("daemon: 创建状态目录: %w", err)
	}
	if err := writeFile0600(config.TokenPath(), token); err != nil {
		ln.Close()
		return err
	}
	if err := writeRuntimeState(os.Getpid(), port); err != nil {
		ln.Close()
		return err
	}
	d.opts.Logf("daemon: 监听 %s (token %s...)", ln.Addr(), token[:8])
	return nil
}

// Addr 返回实际监听地址(测试用 127.0.0.1:0 时端口在此确定)。
func (d *Daemon) Addr() net.Addr {
	if d.ln == nil {
		return nil
	}
	return d.ln.Addr()
}

// Serve 接受连接直到 ctx 取消或 Stop 被调用。
func (d *Daemon) Serve(ctx context.Context) error {
	if d.ln == nil {
		return fmt.Errorf("daemon: 尚未 Listen")
	}
	go d.idleWatch(ctx)

	// ctx 取消时停止接受新连接。
	go func() {
		select {
		case <-ctx.Done():
			d.Stop()
		case <-d.quit:
		}
	}()

	for {
		conn, err := d.ln.Accept()
		if err != nil {
			select {
			case <-d.quit:
				d.wg.Wait()
				return nil
			default:
			}
			return fmt.Errorf("daemon: 接受连接: %w", err)
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handle(conn)
		}()
	}
}

// Stop 关闭监听并断开所有客户端,等待处理协程退出。
func (d *Daemon) Stop() {
	d.quitOnce.Do(func() {
		close(d.quit)
		if d.ln != nil {
			d.ln.Close()
		}
		d.mu.Lock()
		for c := range d.clients {
			c.conn.Close()
		}
		d.mu.Unlock()
		if d.router != nil {
			d.router.Close()
		}
		ClearRuntimeState()
	})
}

// touch 记录一次活动,重置空闲计时。
func (d *Daemon) touch() { d.lastActivity.Store(time.Now().UnixNano()) }

// idleWatch 在空闲超时后自动退出(README §3:无客户端、无活动任务)。
func (d *Daemon) idleWatch(ctx context.Context) {
	timeout := d.opts.IdleTimeout
	if timeout <= 0 {
		return
	}
	// 检查间隔不超过超时的一半,保证退出延迟可控。
	interval := timeout / 2
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.quit:
			return
		case <-t.C:
			if d.clientCount() > 0 || d.activeTasks.Load() > 0 {
				d.touch()
				continue
			}
			if time.Since(time.Unix(0, d.lastActivity.Load())) >= timeout {
				d.opts.Logf("daemon: 空闲超过 %s,退出", timeout)
				d.Stop()
				return
			}
		}
	}
}

func (d *Daemon) clientCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.clients)
}

// handle 处理一条客户端连接:首字节校验 token,随后进入 JSON 行请求循环。
func (d *Daemon) handle(rawConn net.Conn) {
	defer rawConn.Close()
	d.touch()

	c := &clientConn{conn: rawConn, daemon: d}
	if err := c.handshake(); err != nil {
		d.opts.Logf("daemon: 客户端鉴权失败: %v", err)
		return
	}

	d.mu.Lock()
	d.clients[c] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.clients, c)
		d.mu.Unlock()
		d.touch()
	}()

	c.serve()
}

// clientConn 是一条已鉴权的客户端连接。
//
// reader 必须在握手与请求循环之间复用:bufio.Reader 会预读并缓冲字节,
// 若握手用一个、请求循环再新建一个,前者的预读数据就永久丢失(请求会挂起)。
type clientConn struct {
	conn   net.Conn
	daemon *Daemon

	reader *protocol.Reader
	writer *protocol.Writer
	wmu    sync.Mutex // 串行化写回(README §8:并发请求的响应不可交错)

	// 推送订阅(session/tail)。key 形如 "session:sess-1" / "tail:tail-1",
	// value 是取消订阅函数;连接断开时统一取消,避免向死连接推送。
	subMu sync.Mutex
	subs  map[string]func()
}

// handshake 读取首行 token 并比对。失败即断开,不做任何响应,避免向未鉴权方泄露信息。
func (c *clientConn) handshake() error {
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	c.reader = protocol.NewReader(c.conn)
	token, err := c.reader.ReadTokenLine()
	if err != nil {
		return fmt.Errorf("读取 token: %w", err)
	}
	_ = c.conn.SetReadDeadline(time.Time{})
	if token != c.daemon.token {
		return errors.New("token 不匹配")
	}
	c.writer = protocol.NewWriter(c.conn)
	return nil
}

// serve 读取请求并逐个分发处理。
func (c *clientConn) serve() {
	// 连接级 ctx 在连接断开时取消;请求各自派生,避免长驻订阅随请求返回而失效。
	baseCtx, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()
	// 连接关闭或 daemon 退出时取消所有在途请求/订阅。
	go func() {
		select {
		case <-c.daemon.quit:
			baseCancel()
		case <-baseCtx.Done():
		}
	}()

	for {
		req, err := c.reader.ReadRequest()
		if err != nil {
			break // EOF 或读错误,连接结束
		}
		c.daemon.touch()
		// 每个请求独立 goroutine,支持同连接并发请求(响应按 id 关联)。
		go c.dispatch(baseCtx, req)
	}

	// 连接结束:取消本连接的全部推送订阅(会话/ tail),防止向死连接写。
	c.cancelSubs()
}

// dispatch 路由单条请求并把结果写回。
//
// emit 把任务进度事件写回本连接(以请求 id 关联);事件与响应共用写锁,
// 保证每条消息完整、不交错。写事件失败(客户端已断开)只记日志,不影响任务本身。
func (c *clientConn) dispatch(ctx context.Context, req *protocol.Request) {
	resp := c.daemon.router.Handle(ctx, req, c)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.writer.WriteResponse(resp); err != nil {
		c.daemon.opts.Logf("daemon: 写响应失败: %v", err)
		return
	}
	if err := c.writer.Flush(); err != nil {
		c.daemon.opts.Logf("daemon: 刷写响应失败: %v", err)
	}
}

// Event 实现 router.Conn:把事件写回本连接(以请求 id 关联),并发安全。
func (c *clientConn) Event(id uint64, event string, data any) error {
	ev, err := protocol.NewEvent(id, event, data)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.writer.WriteEvent(ev); err != nil {
		return err
	}
	return c.writer.Flush()
}

// AddSub 实现 router.Conn:登记连接级订阅(会话输出 / tail 行)。
func (c *clientConn) AddSub(key string, cancel func()) { c.addSub(key, cancel) }

// DelSub 实现 router.Conn:取消并移除一个订阅。
func (c *clientConn) DelSub(key string) { c.delSub(key) }

// addSub 登记一个连接级订阅(会话输出 / tail 行)。key 必须唯一。
func (c *clientConn) addSub(key string, cancel func()) {
	c.subMu.Lock()
	if c.subs == nil {
		c.subs = map[string]func(){}
	}
	if old, ok := c.subs[key]; ok {
		old() // 同 key 重复订阅时先取消旧的,避免泄漏
	}
	c.subs[key] = cancel
	c.subMu.Unlock()
}

// delSub 取消并移除一个订阅。
func (c *clientConn) delSub(key string) {
	c.subMu.Lock()
	cancel, ok := c.subs[key]
	delete(c.subs, key)
	c.subMu.Unlock()
	if ok {
		cancel()
	}
}

// cancelSubs 取消本连接的全部订阅(连接断开时调用)。
func (c *clientConn) cancelSubs() {
	c.subMu.Lock()
	subs := c.subs
	c.subs = nil
	c.subMu.Unlock()
	for _, cancel := range subs {
		cancel()
	}
}
