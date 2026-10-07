package sshtest

import (
	"io"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Throttle 按字节速率限制 SFTP 通道上的入站流量,用于模拟慢机
// (对应真实环境里 `tc` 限速或低配容器)。零值不做限制。
type Throttle struct {
	// BytesPerSec 是限速字节数/秒。
	BytesPerSec int64

	mu     sync.Mutex
	last   time.Time
	credit float64
}

// wait 消耗 n 字节的额度,不足时按速率睡眠补足。
func (t *Throttle) wait(n int) {
	if t == nil || t.BytesPerSec <= 0 || n <= 0 {
		return
	}
	t.mu.Lock()
	now := time.Now()
	if t.last.IsZero() {
		t.last = now
	}
	t.credit += now.Sub(t.last).Seconds() * float64(t.BytesPerSec)
	t.last = now
	t.credit -= float64(n)
	var d time.Duration
	if t.credit < 0 {
		d = time.Duration(-t.credit / float64(t.BytesPerSec) * float64(time.Second))
		t.credit = 0
	}
	t.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

// throttledChannel 在 ssh.Channel 的读路径上注入限速。
type throttledChannel struct {
	ssh.Channel
	t *Throttle
}

func (c *throttledChannel) Read(b []byte) (int, error) {
	n, err := c.Channel.Read(b)
	if n > 0 {
		c.t.wait(n)
	}
	return n, err
}

// newThrottledServer 构造 SFTP 服务端;t 非空时限速其读入流量。
func newThrottledServer(ch ssh.Channel, t *Throttle, opts ...sftp.ServerOption) (*sftp.Server, error) {
	var rwc io.ReadWriteCloser = ch
	if t != nil && t.BytesPerSec > 0 {
		rwc = &throttledChannel{Channel: ch, t: t}
	}
	return sftp.NewServer(rwc, opts...)
}
