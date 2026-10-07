package pool

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/singleflight"

	"github.com/xelr233/sshfleet/internal/auth"
	"github.com/xelr233/sshfleet/internal/config"
)

// keepaliveRequestType 是 OpenSSH 约定的保活请求类型;服务端回 ssh.MsgRequestSuccess。
const keepaliveRequestType = "keepalive@openssh.com"

// Options 是连接池的可调项。零值可通过 withDefaults 得到一套可用默认。
type Options struct {
	// DialTimeout 限制单次 TCP 连接 + SSH 握手的总时长。
	DialTimeout time.Duration
	// KeepaliveInterval 是保活探测间隔;0 表示禁用保活(仍会监听连接断开)。
	KeepaliveInterval time.Duration
	// MaxChannels 是每主机并发 SSH channel 上限(README §5.3,默认 4)。
	MaxChannels int
	// MaxDialAttempts 是单次 Get 内 dial 的最大尝试次数。0 用默认(3);负数表示不限
	// (仅受 ctx 约束)。
	MaxDialAttempts int
	// BackoffBase / BackoffMax 是重连退避的起始与上限(README §5.8)。
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// OnWarn 接收非致命告警(如 agent 降级、host key 未校验)。
	OnWarn func(format string, args ...any)
}

func (o Options) withDefaults() Options {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 15 * time.Second
	}
	if o.KeepaliveInterval < 0 {
		o.KeepaliveInterval = 0
	} else if o.KeepaliveInterval == 0 {
		o.KeepaliveInterval = 30 * time.Second
	}
	if o.MaxChannels <= 0 {
		o.MaxChannels = 4
	}
	// 默认有限重试:某主机彻底不可达时应尽快返回明确错误,而不是无限重试挂住请求
	// (README §5.8:任务失败由上层决定是否重试)。
	if o.MaxDialAttempts == 0 {
		o.MaxDialAttempts = 3
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 100 * time.Millisecond
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 30 * time.Second
	}
	return o
}

// Stat 是单台主机的池状态快照(供 daemon 的 status 命令展示)。
type Stat struct {
	Host       string
	Connected  bool
	Channels   int
	DialedAt   time.Time
	Reconnects int
	Warnings   []string
}

// Pool 是 SSH 连接池。它按主机名缓存 *ssh.Client,对同一主机的并发 dial 去重,
// 并在连接断开时于下次取用时自动重连(README §5.8)。
//
// Pool 自身并发安全。
type Pool struct {
	cfg  *config.File
	opts Options

	sf singleflight.Group

	mu      sync.Mutex
	entries map[string]*entry

	done   chan struct{}
	closed atomic.Bool
}

// entry 是每主机的稳定记录:即使 Conn 被替换,reconnects / warnings 也保留。
type entry struct {
	mu         sync.Mutex
	conn       *Conn
	reconnects int
	warnings   []string
}

// New 构造连接池。cfg 提供主机配置(含凭据与 shell 设定)。
func New(cfg *config.File, opts Options) *Pool {
	return &Pool{
		cfg:     cfg,
		opts:    opts.withDefaults(),
		entries: map[string]*entry{},
		done:    make(chan struct{}),
	}
}

// Get 返回指定主机的可用连接;命中缓存直接返回,否则 dial(并发去重 + 退避重试)。
// ctx 约束 dial 与重试的总时长。
func (p *Pool) Get(ctx context.Context, name string) (*Conn, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("pool: 连接池已关闭")
	}
	h, err := p.host(name)
	if err != nil {
		return nil, err
	}
	if c := p.cached(name); c != nil {
		return c, nil
	}

	// singleflight:同一主机的并发 miss 只触发一次 dial,避免惊群(README §8)。
	ch := p.sf.DoChan(name, func() (any, error) {
		return p.dialWithBackoff(ctx, name, h)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*Conn), nil
	}
}

// Stats 返回所有已知主机的池状态快照,按主机名排序。
func (p *Pool) Stats() []Stat {
	p.mu.Lock()
	names := make([]string, 0, len(p.entries))
	for n := range p.entries {
		names = append(names, n)
	}
	p.mu.Unlock()
	sort.Strings(names)

	out := make([]Stat, 0, len(names))
	for _, n := range names {
		e := p.entryOf(n)
		e.mu.Lock()
		st := Stat{Host: n, Reconnects: e.reconnects, Warnings: append([]string(nil), e.warnings...)}
		if c := e.conn; c != nil && !c.closed.Load() {
			st.Connected = true
			st.Channels = c.InUse()
			st.DialedAt = c.createdAt
		}
		e.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// Close 关闭池中所有连接并阻止后续 Get。
func (p *Pool) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.done)
	p.mu.Lock()
	entries := make([]*entry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		if e.conn != nil {
			e.conn.Close()
		}
		e.mu.Unlock()
	}
	return nil
}

// host 按名解析主机配置。
func (p *Pool) host(name string) (*config.Host, error) {
	if p.cfg == nil {
		return nil, fmt.Errorf("pool: 未提供主机配置")
	}
	h, ok := p.cfg.Hosts[name]
	if !ok {
		return nil, fmt.Errorf("pool: 未知主机 %q", name)
	}
	return h, nil
}

// cached 返回未失效的缓存连接;已失效的连接返回 nil,但不清空字段 ——
// 保留它让 dialWithBackoff 能识别出「这是一次重连」并计数。
func (p *Pool) cached(name string) *Conn {
	e := p.entryOf(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil || e.conn.closed.Load() {
		return nil
	}
	return e.conn
}

// entryOf 取出(必要时创建)主机记录。
func (p *Pool) entryOf(name string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[name]
	if e == nil {
		e = &entry{}
		p.entries[name] = e
	}
	return e
}

// dialWithBackoff 在 e.mu 保护下尝试建立连接,失败按退避重试。
func (p *Pool) dialWithBackoff(ctx context.Context, name string, h *config.Host) (*Conn, error) {
	e := p.entryOf(name)
	e.mu.Lock()
	defer e.mu.Unlock()

	// 双重检查:可能在等锁期间已有其它调用者建好连接。
	if e.conn != nil && !e.conn.closed.Load() {
		return e.conn, nil
	}
	// 记录本次是否为「替换一条已失效的连接」,用于重连计数。
	prev := e.conn

	backoff := p.opts.BackoffBase
	var lastErr error
	for attempt := 0; p.opts.MaxDialAttempts < 0 || attempt < p.opts.MaxDialAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("pool: 重连 %s 前被取消: %w(最后错误: %v)", name, ctx.Err(), lastErr)
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff, p.opts.BackoffMax)
		}
		c, warns, err := p.dialOnce(ctx, h)
		if err != nil {
			lastErr = err
			p.warn("pool: dial %s 失败(第 %d 次): %v", name, attempt+1, err)
			continue
		}
		if prev != nil { // 之前有连接且已失效 → 这是一次重连
			e.reconnects++
		}
		e.conn = c
		e.warnings = warns
		for _, w := range warns {
			p.warn("pool: %s: %s", name, w)
		}
		go c.watch(p)
		return c, nil
	}
	return nil, fmt.Errorf("pool: %s 连接失败,已放弃(%d 次): %w", name, p.opts.MaxDialAttempts, lastErr)
}

// dialOnce 做一次 TCP 连接 + SSH 握手,成功返回带 channel 限流的 Conn。
func (p *Pool) dialOnce(ctx context.Context, h *config.Host) (*Conn, []string, error) {
	cfg, warns, err := auth.BuildClientConfig(h, auth.ConfigOptions{Timeout: p.opts.DialTimeout})
	if err != nil {
		return nil, warns, err
	}
	addr := net.JoinHostPort(h.Addr, strconv.Itoa(h.Port))

	netConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, warns, fmt.Errorf("连接 %s: %w", addr, err)
	}
	// 握手也受 DialTimeout 约束;握手完成后必须清除 deadline,
	// 否则连接上后续所有读写都会在同一个绝对时刻超时。
	if p.opts.DialTimeout > 0 {
		if err := netConn.SetDeadline(time.Now().Add(p.opts.DialTimeout)); err != nil {
			netConn.Close()
			return nil, warns, fmt.Errorf("设置握手超时 %s: %w", addr, err)
		}
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		netConn.Close()
		return nil, warns, fmt.Errorf("SSH 握手 %s: %w", addr, err)
	}
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, warns, fmt.Errorf("清除握手超时 %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	return newConn(h, client, p.opts.MaxChannels), warns, nil
}

func (p *Pool) warn(format string, args ...any) {
	if p.opts.OnWarn != nil {
		p.opts.OnWarn(format, args...)
	}
}
