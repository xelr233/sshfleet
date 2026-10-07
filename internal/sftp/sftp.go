// Package sftp 提供单机 SFTP 传输(上传/下载),带进度回调与断点续传支持。
//
// 它只负责「字节流 ↔ 远端文件」,不感知分发任务:distribute 复用本包的
// Client(每目标机一个),对同一个已打开的本地文件各取独立读游标,从而
// 慢机只拖慢自己(见 internal/dist)。
//
// 每个 Client 占用连接池的一个 channel 名额(Open 时 Acquire,Close 时 Release),
// 因此天然受每主机并发上限约束(README §5.3)。
package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"

	pkgsftp "github.com/pkg/sftp"

	"github.com/xelr233/sshfleet/internal/pool"
)

// ChunkSize 是单次读写的缓冲大小。256KiB 在吞吐与内存之间取平衡:
// 足够摊薄每包的往返开销,又不至于让多机并发的峰值内存过高。
const ChunkSize = 256 << 10

// DefaultProgressInterval 是进度回调的最小间隔(避免每块都回调,刷屏且耗 CPU)。
const DefaultProgressInterval = 200 * time.Millisecond

// Client 是一条已建立的 SFTP 会话,绑定到一个连接池 channel 名额。
type Client struct {
	sc   *pkgsftp.Client
	conn *pool.Conn
	host string

	closed bool
}

// Open 在 conn 上建立 SFTP 会话。会占用一个 channel 名额,使用后必须 Close。
func Open(ctx context.Context, conn *pool.Conn) (*Client, error) {
	if err := conn.Acquire(ctx); err != nil {
		return nil, err
	}
	sc, err := pkgsftp.NewClient(conn.Client())
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("sftp: 建立会话: %w", err)
	}
	host := "?"
	if h := conn.Host(); h != nil {
		host = h.Name
	}
	return &Client{sc: sc, conn: conn, host: host}, nil
}

// Host 返回该会话对应主机名(便于错误信息)。
func (c *Client) Host() string { return c.host }

// Close 关闭 SFTP 会话并归还 channel 名额;可重复调用。
func (c *Client) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.sc.Close()
	c.conn.Release()
	return err
}

// RemoteSize 返回远端文件大小;文件不存在时返回 -1 且 err 为 nil。
// 用于断点续传时判断已写入的字节数。
func (c *Client) RemoteSize(remote string) (int64, error) {
	fi, err := c.sc.Stat(remote)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -1, nil
		}
		return 0, fmt.Errorf("sftp: stat %s: %w", remote, err)
	}
	return fi.Size(), nil
}

// PutOptions 控制一次上传。
type PutOptions struct {
	// Offset 是远端文件的起始写偏移;调用方需保证 src 已定位到同一偏移。
	// 用于断点续传。0 表示从头发起(并截断已有文件)。
	Offset int64
	// Total 是文件总大小,仅用于进度分母(<=0 时进度只有已写字节数)。
	Total int64
	// Mkdir 为真时自动创建远端父目录。
	Mkdir bool
	// ProgressInterval 是进度回调最小间隔;<=0 用 DefaultProgressInterval。
	ProgressInterval time.Duration
	// OnProgress 在传输过程中被调用(offset 为累计已写字节数,含 Offset)。
	OnProgress func(offset, total int64)
}

// Put 把 src 写入远端 remote。返回本次调用实际写入的字节数(不含 Offset)。
//
// src 会被读到 EOF;若中途 ctx 取消或写入失败则返回错误,远端可能留下部分内容,
// 由上层记录 offset 以便 resume。
func (c *Client) Put(ctx context.Context, src io.Reader, remote string, opts PutOptions) (int64, error) {
	if opts.Mkdir {
		if dir := path.Dir(remote); dir != "" && dir != "." && dir != "/" {
			// 父目录已存在不是错误,其它错误留给 OpenFile 报出更准确的上下文。
			if err := c.sc.MkdirAll(dir); err != nil && !errors.Is(err, fs.ErrExist) {
				_ = err
			}
		}
	}

	flags := os.O_WRONLY | os.O_CREATE
	if opts.Offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := c.sc.OpenFile(remote, flags)
	if err != nil {
		return 0, fmt.Errorf("sftp: 打开远端 %s: %w", remote, err)
	}
	defer f.Close()

	if opts.Offset > 0 {
		// 续传:把可能残留的多余尾部截掉,再从 offset 继续写,避免新旧内容交错。
		if err := f.Truncate(opts.Offset); err != nil {
			return 0, fmt.Errorf("sftp: 截断 %s 到 %d: %w", remote, opts.Offset, err)
		}
		if _, err := f.Seek(opts.Offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("sftp: 定位 %s@%d: %w", remote, opts.Offset, err)
		}
	}

	// ctx 取消时关闭会话,解除可能阻塞中的读写(SSH 通道关闭会让读/写返回错误)。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.sc.Close()
		case <-stop:
		}
	}()

	prog := newReporter(opts)
	buf := make([]byte, ChunkSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				if ctx.Err() != nil {
					return written, ctx.Err()
				}
				return written, fmt.Errorf("sftp: 写 %s: %w", remote, werr)
			}
			written += int64(n)
			prog.report(opts.Offset + written)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, fmt.Errorf("sftp: 读取源数据: %w", rerr)
		}
	}
	prog.flush(opts.Offset + written)
	return written, nil
}

// Get 把远端 remote 读入 dst。返回写入 dst 的字节数。
func (c *Client) Get(ctx context.Context, dst io.Writer, remote string, opts PutOptions) (int64, error) {
	f, err := c.sc.Open(remote)
	if err != nil {
		return 0, fmt.Errorf("sftp: 打开远端 %s: %w", remote, err)
	}
	defer f.Close()

	total := opts.Total
	if total <= 0 {
		if fi, err := f.Stat(); err == nil {
			total = fi.Size()
		}
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.sc.Close()
		case <-stop:
		}
	}()

	prog := newReporter(opts)
	prog.setTotal(total)
	buf := make([]byte, ChunkSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, fmt.Errorf("sftp: 写本地: %w", werr)
			}
			written += int64(n)
			prog.report(written)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, fmt.Errorf("sftp: 读远端 %s: %w", remote, rerr)
		}
	}
	prog.flush(written)
	return written, nil
}

// reporter 按最小间隔节流进度回调。
type reporter struct {
	fn    func(offset, total int64)
	min   time.Duration
	last  time.Time
	total int64
}

func newReporter(opts PutOptions) *reporter {
	interval := opts.ProgressInterval
	if interval <= 0 {
		interval = DefaultProgressInterval
	}
	return &reporter{fn: opts.OnProgress, min: interval, total: opts.Total}
}

func (r *reporter) setTotal(total int64) { r.total = total }

func (r *reporter) report(offset int64) {
	if r.fn == nil {
		return
	}
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < r.min {
		return
	}
	r.last = now
	r.fn(offset, r.total)
}

// flush 强制发出一次进度(传输结束时的最终值,不受节流限制)。
func (r *reporter) flush(offset int64) {
	if r.fn != nil {
		r.fn(offset, r.total)
	}
}
