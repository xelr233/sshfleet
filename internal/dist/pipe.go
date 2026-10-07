package dist

import (
	"context"
	"io"
	"sync"
)

// pipeBuffer 是一个带「预读窗口」的有界内存管道:它连接单一的写者(广播器)
// 与单一的读者(某台机器的 SFTP 写入)。当缓冲达到 max 字节时,写者阻塞,
// 直到读者消费出空间。这正是 README §5.4 所说的「pipe 缓冲即流控边界」:
// 慢机只把自己的缓冲填满,广播器至多领先它 max 字节,快机不受影响。
//
// Write 会复制传入的字节(广播器的 chunk 缓冲会被复用),因此调用方可安全复用 buf。
type pipeBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	max    int
	closed bool
	err    error // 非 nil 时,读者在耗尽缓冲后读到该错误而不是 io.EOF
}

func newPipeBuffer(max int) *pipeBuffer {
	p := &pipeBuffer{max: max}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Read 从缓冲取数据;缓冲为空时阻塞,直到有数据或管道关闭。
func (p *pipeBuffer) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 {
		if p.closed {
			if p.err != nil {
				return 0, p.err
			}
			return 0, io.EOF
		}
		p.cond.Wait()
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	if len(p.buf) == 0 {
		p.buf = nil
	}
	p.cond.Broadcast() // 释放被预读窗口挡住的写者
	return n, nil
}

// Write 追加数据;缓冲达到上限时阻塞,直到读者腾出空间或管道关闭。
func (p *pipeBuffer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	for p.max > 0 && len(p.buf) >= p.max {
		if p.closed {
			return 0, io.ErrClosedPipe
		}
		p.cond.Wait()
	}
	// append 会复制字节,调用方的 chunk 缓冲可安全复用。
	p.buf = append(p.buf, b...)
	p.cond.Broadcast()
	return len(b), nil
}

// Close 标记管道结束:读者耗尽缓冲后读到 io.EOF。重复调用无副作用。
func (p *pipeBuffer) Close() error { return p.CloseWithError(nil) }

// CloseWithError 同 Close,但读者最终读到 err(用于把广播错误或取消传播给消费者)。
func (p *pipeBuffer) CloseWithError(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if err != nil {
		p.err = err
	}
	p.cond.Broadcast()
	return nil
}

// broadcaster 把一个字节流广播到多个 pipeBuffer。每个订阅者看到完整、等长的
// 字节序列;任一订阅者失败(其读端关闭)后不再向它写入,且不影响其它订阅者。
type broadcaster struct {
	mu    sync.Mutex
	pipes []*pipeBuffer
}

func newBroadcaster() *broadcaster { return &broadcaster{} }

// subscribe 新建一条订阅管道(预读窗口为 readahead 字节)。
func (b *broadcaster) subscribe() *pipeBuffer {
	p := newPipeBuffer(readahead)
	b.mu.Lock()
	b.pipes = append(b.pipes, p)
	b.mu.Unlock()
	return p
}

// broadcast 把 chunk 写给所有仍然可写的订阅者,返回成功写入的数量。
// 已关闭(失败)的订阅者被跳过,不让单机失败拖垮整体。
func (b *broadcaster) broadcast(chunk []byte) int {
	b.mu.Lock()
	pipes := append([]*pipeBuffer(nil), b.pipes...)
	b.mu.Unlock()
	live := 0
	for _, p := range pipes {
		if _, err := p.Write(chunk); err != nil {
			continue // 该机读端已关闭,跳过
		}
		live++
	}
	return live
}

// closeAll 以 EOF 结束所有订阅者(正常读完源文件时)。
func (b *broadcaster) closeAll() {
	b.mu.Lock()
	pipes := b.pipes
	b.pipes = nil
	b.mu.Unlock()
	for _, p := range pipes {
		p.Close()
	}
}

// abort 以错误结束所有订阅者(源读取失败或任务取消时)。
func (b *broadcaster) abort(err error) {
	b.mu.Lock()
	pipes := b.pipes
	b.pipes = nil
	b.mu.Unlock()
	for _, p := range pipes {
		p.CloseWithError(err)
	}
}

// readAll 从 src 顺序读取并广播,直到 EOF、出错或 ctx 取消。
// 所有订阅者都失败时提前停止读盘,避免为无人接收的数据空转。
func (b *broadcaster) readAll(ctx context.Context, src io.Reader, total int64) {
	buf := make([]byte, ChunkSize)
	var sent int64
	for {
		if err := ctx.Err(); err != nil {
			b.abort(err)
			return
		}
		if total > 0 && sent >= total {
			break
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if live := b.broadcast(buf[:n]); live == 0 {
				return // 所有目标都已失败,无需继续读盘
			}
			sent += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			b.abort(rerr)
			return
		}
	}
	b.closeAll()
}
