// Package dist 实现单文件向多机的扇出分发(distribute),是本项目的核心难点。
//
// 设计(README §5.4):
//
//	磁盘只读一次 → 顺序预读 → chunk 广播到每台目标机独立的带缓冲管道
//	→ 每机 goroutine 从自己的管道读并 SFTP 写入。
//
// 管道缓冲即流控边界:慢机只堵住自己的缓冲,广播器至多领先它一个预读窗口,
// 快机照常前进。逐机记录 offset,失败机器可用 Resume 从 offset 续传。
package dist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sftp"
)

// DefaultConcurrency 是分发未指定并发度时的默认值。
const DefaultConcurrency = 16

// ChunkSize 是广播的块大小。
const ChunkSize = 256 << 10

// readahead 是广播器相对单个消费者的最大领先字节数,决定「慢机最多堵多久」。
// 超过它广播器暂停从磁盘读:内存有界,同时允许快机领先慢机这么多数据。
const readahead = 4 << 20 // 4 MiB

// State 是单机分发的状态。
type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// HostProgress 是单机的分发进度快照。
type HostProgress struct {
	Host   string
	State  State
	Offset int64
	Total  int64
	Err    string
}

// Options 是一次分发的参数。
type Options struct {
	// LocalPath 是本地源文件。
	LocalPath string
	// RemotePath 是远端目标路径(每台机器相同)。
	RemotePath string
	// Concurrency 是同时在传的主机数上限;<=0 用 DefaultConcurrency。
	Concurrency int
	// Resume 为真时,已有远端文件从其后缀续传(而不是从头覆盖)。
	Resume bool
	// Mkdir 为真时自动创建远端父目录。
	Mkdir bool
	// OnProgress 在进度变化时被调用(offset 递增、状态切换)。
	OnProgress func(HostProgress)
}

// Result 是一次分发的最终结果。
type Result struct {
	TaskID   string
	Hosts    []HostProgress
	Elapsed  time.Duration
	Canceled bool
}

// Task 是一次分发的运行体。自包含:给定连接池与主机名即可完成广播与逐机写入,
// 并通过 Progress()/Wait() 暴露状态,供 daemon 的 dist_status 查询。
type Task struct {
	id         string
	opts       Options
	remotePath string
	order      []string // 主机名,排序后固定顺序

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	hosts    map[string]*HostProgress
	started  time.Time
	elapsed  time.Duration
	canceled bool
	finished bool
	done     chan struct{}
}

// NewTask 构造一次分发。taskID 由调用方(daemon)分配,用于 dist_status 关联。
func NewTask(taskID string, hostNames []string, opts Options) *Task {
	ctx, cancel := context.WithCancel(context.Background())
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	order := append([]string(nil), hostNames...)
	sort.Strings(order)

	t := &Task{
		id:         taskID,
		opts:       opts,
		remotePath: opts.RemotePath,
		order:      order,
		ctx:        ctx,
		cancel:     cancel,
		hosts:      make(map[string]*HostProgress, len(order)),
		done:       make(chan struct{}),
	}
	for _, h := range order {
		t.hosts[h] = &HostProgress{Host: h, State: StatePending}
	}
	return t
}

// ID 返回任务 id。
func (t *Task) ID() string { return t.id }

// Cancel 请求取消:中止广播与所有在途写入。
func (t *Task) Cancel() { t.cancel() }

// Progress 返回整体状态、逐机进度快照与完成/失败计数。
func (t *Task) Progress() (state string, hosts []HostProgress, done, failed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	hosts = make([]HostProgress, 0, len(t.order))
	for _, name := range t.order {
		hp := *t.hosts[name]
		hosts = append(hosts, hp)
		switch hp.State {
		case StateDone:
			done++
		case StateFailed:
			failed++
		}
	}
	switch {
	case !t.finished:
		state = "running"
	case t.canceled:
		state = "canceled"
	default:
		state = "done"
	}
	return state, hosts, done, failed
}

// Wait 阻塞到任务结束并返回最终结果。可安全地多次调用。
func (t *Task) Wait() Result {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	res := Result{TaskID: t.id, Elapsed: t.elapsed, Canceled: t.canceled}
	for _, name := range t.order {
		res.Hosts = append(res.Hosts, *t.hosts[name])
	}
	return res
}

// Run 执行分发:打开源文件、建立广播者、为每台机器起一个写入 goroutine,
// 当前 goroutine 负责从磁盘读并广播。返回即任务结束(此时 Wait 不再阻塞)。
func (t *Task) Run(p *pool.Pool) {
	t.mu.Lock()
	t.started = time.Now()
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.finished = true
		t.elapsed = time.Since(t.started)
		t.canceled = t.ctx.Err() != nil
		close(t.done)
		t.mu.Unlock()
		t.cancel()
	}()

	src, err := os.Open(t.opts.LocalPath)
	if err != nil {
		t.failAll(fmt.Errorf("打开本地文件: %w", err))
		t.emitAll()
		return
	}
	defer src.Close()

	fi, err := src.Stat()
	if err != nil {
		t.failAll(fmt.Errorf("读取本地文件信息: %w", err))
		t.emitAll()
		return
	}
	total := fi.Size()
	t.setTotal(total)

	bc := newBroadcaster()
	// 每机一条订阅管道;消费端 goroutine 负责从管道读并 SFTP 写入。
	readers := make([]*pipeBuffer, 0, len(t.order))
	for range t.order {
		readers = append(readers, bc.subscribe())
	}

	var wg sync.WaitGroup
	// 并发度只限制「同时建立连接」的数量,不限制消费本身:扇出是单次读盘广播,
	// 任何消费端若被挡在门外不读自己的管道,都会反压广播器并拖住所有人(甚至死锁)。
	// 限制建连数量即可避免上百台机器同时握手的突发。
	dialSem := make(chan struct{}, t.opts.Concurrency)
	for i, name := range t.order {
		wg.Add(1)
		go func(name string, r *pipeBuffer) {
			defer wg.Done()
			// 无论成功失败,退出时关闭读端,让广播器立刻感知并跳过本机,
			// 避免它继续往无人消费的管道里写而阻塞。
			defer r.CloseWithError(fmt.Errorf("dist: %s 写入已结束", name))
			t.sendOne(p, name, r, total, dialSem)
		}(name, readers[i])
	}

	// 读盘 + 广播;它在所有消费端就绪后推进,慢机通过管道反压它。
	bc.readAll(t.ctx, src, total)
	wg.Wait()
	bc.closeAll()
}

// sendOne 在一台机器上完成写入:取连接、开 SFTP、从管道读到结束。
// dialSem 限制同时建连数量(拿号后立即归还,不覆盖传输阶段)。
func (t *Task) sendOne(p *pool.Pool, name string, r *pipeBuffer, total int64, dialSem chan struct{}) {
	select {
	case dialSem <- struct{}{}:
	case <-t.ctx.Done():
		t.fail(name, "已取消(尚未开始)")
		t.emit(name)
		return
	}
	conn, err := p.Get(t.ctx, name)
	<-dialSem
	if err != nil {
		t.fail(name, "取连接: "+err.Error())
		t.emit(name)
		return
	}
	cli, err := sftp.Open(t.ctx, conn)
	if err != nil {
		t.fail(name, "建立 SFTP 会话: "+err.Error())
		t.emit(name)
		return
	}
	defer cli.Close()

	var offset int64
	if t.opts.Resume && total > 0 {
		if sz, err := cli.RemoteSize(t.remotePath); err == nil && sz > 0 && sz <= total {
			offset = sz
			// 丢弃管道中的前 offset 字节:它们已在远端存在。
			if _, err := io.CopyN(io.Discard, r, offset); err != nil {
				t.fail(name, "跳过已传部分: "+err.Error())
				t.emit(name)
				return
			}
		}
	}

	t.setState(name, StateRunning, total, offset, "")
	t.emit(name)

	n, err := cli.Put(t.ctx, r, t.remotePath, sftp.PutOptions{
		Offset: offset,
		Total:  total,
		Mkdir:  t.opts.Mkdir,
		OnProgress: func(off, _ int64) {
			t.setOffset(name, off)
			t.emit(name)
		},
	})
	if err != nil {
		if t.ctx.Err() != nil || errors.Is(err, context.Canceled) {
			t.fail(name, fmt.Sprintf("已取消(已传 %d 字节)", offset+n))
		} else {
			t.fail(name, err.Error())
		}
		t.emit(name)
		return
	}
	t.setState(name, StateDone, total, offset+n, "")
	t.emit(name)
}

// ---- 状态维护(均在 t.mu 下更新 t.hosts,单一事实来源) ----

func (t *Task) setTotal(total int64) {
	t.mu.Lock()
	for _, hp := range t.hosts {
		hp.Total = total
	}
	t.mu.Unlock()
}

func (t *Task) setState(name string, st State, total, offset int64, msg string) {
	t.mu.Lock()
	hp := t.hosts[name]
	hp.State, hp.Total, hp.Offset, hp.Err = st, total, offset, msg
	t.mu.Unlock()
}

func (t *Task) setOffset(name string, off int64) {
	t.mu.Lock()
	t.hosts[name].Offset = off
	t.mu.Unlock()
}

func (t *Task) fail(name, msg string) {
	t.mu.Lock()
	hp := t.hosts[name]
	hp.State, hp.Err = StateFailed, msg
	t.mu.Unlock()
}

// failAll 把所有未完成的主机标记为失败(源文件无法打开等整体性错误)。
func (t *Task) failAll(err error) {
	t.mu.Lock()
	for _, hp := range t.hosts {
		if hp.State != StateDone {
			hp.State, hp.Err = StateFailed, err.Error()
		}
	}
	t.mu.Unlock()
}

// snapshot 复制某机当前进度(带锁),用于回调,避免把锁暴露给外部代码。
func (t *Task) snapshot(name string) HostProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *t.hosts[name]
}

func (t *Task) emit(name string) {
	if t.opts.OnProgress == nil {
		return
	}
	t.opts.OnProgress(t.snapshot(name))
}

func (t *Task) emitAll() {
	if t.opts.OnProgress == nil {
		return
	}
	for _, name := range t.order {
		t.emit(name)
	}
}
