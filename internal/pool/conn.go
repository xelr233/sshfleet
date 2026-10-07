package pool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/xelr233/sshfleet/internal/config"
)

// Conn 是池中一条已建立的 SSH 连接,附带每主机 channel 并发限流。
//
// 禁止在任务间共享单个 SSH channel(README §5.3):调用方通过 Acquire/Release
// 取得一个 channel 名额,再自行 NewSession。
type Conn struct {
	host   *config.Host
	client *ssh.Client
	sem    chan struct{} // 容量即每主机 channel 上限

	createdAt time.Time
	closed    atomic.Bool
	closeErr  atomic.Value // error
}

func newConn(h *config.Host, client *ssh.Client, maxChannels int) *Conn {
	return &Conn{
		host:      h,
		client:    client,
		sem:       make(chan struct{}, maxChannels),
		createdAt: time.Now(),
	}
}

// Host 返回该连接对应的主机配置(含 Shell 设定,供 exec 层决定如何包装命令)。
func (c *Conn) Host() *config.Host { return c.host }

// Client 返回底层 ssh.Client。仅在已 Acquire 的临界区内使用。
func (c *Conn) Client() *ssh.Client { return c.client }

// Closed 报告连接是否已被判定失效。
func (c *Conn) Closed() bool { return c.closed.Load() }

// InUse 返回当前占用的 channel 数。
func (c *Conn) InUse() int { return len(c.sem) }

// CloseErr 返回连接失效原因(未失效时返回 nil)。
func (c *Conn) CloseErr() error {
	if v := c.closeErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// Acquire 占用一个 channel 名额,受 ctx 约束。
func (c *Conn) Acquire(ctx context.Context) error {
	if c.closed.Load() {
		return fmt.Errorf("pool: 连接已失效: %w", c.CloseErr())
	}
	select {
	case c.sem <- struct{}{}:
		// 拿到名额后再次确认连接仍然有效,避免关闭竞态。
		if c.closed.Load() {
			c.Release()
			return fmt.Errorf("pool: 连接已失效: %w", c.CloseErr())
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release 归还一个 channel 名额。
func (c *Conn) Release() {
	select {
	case <-c.sem:
	default:
	}
}

// Close 关闭底层连接。
func (c *Conn) Close() error {
	c.markClosed(fmt.Errorf("pool: 连接被主动关闭"))
	return c.client.Close()
}

// markClosed 记录失效原因(只记录第一次),并唤醒处于 Acquire 竞争中的调用方。
func (c *Conn) markClosed(err error) {
	if c.closed.CompareAndSwap(false, true) {
		if err == nil {
			err = fmt.Errorf("pool: 连接已关闭")
		}
		c.closeErr.Store(err)
	}
}

// watch 监视连接生命周期:连接断开立即感知,并周期性发送 keepalive 探活。
// 任一方式判定失效后标记 closed,连接池会在下次 Get 时重连(README §5.8)。
func (c *Conn) watch(p *Pool) {
	waited := make(chan error, 1)
	go func() { waited <- c.client.Wait() }()

	var tick <-chan time.Time
	if p.opts.KeepaliveInterval > 0 {
		t := time.NewTicker(p.opts.KeepaliveInterval)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case err := <-waited:
			c.markClosed(fmt.Errorf("pool: 连接已断开: %w", err))
			return
		case <-tick:
			// keepalive 走全局请求,不占用 channel 名额。
			if _, _, err := c.client.SendRequest(keepaliveRequestType, true, nil); err != nil {
				c.markClosed(fmt.Errorf("pool: keepalive 失败: %w", err))
				c.client.Close()
				return
			}
		case <-p.done:
			return
		}
	}
}
