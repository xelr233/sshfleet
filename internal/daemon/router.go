package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/exec"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/protocol"
)

// EmitFunc 由 daemon 注入,用于把任务进度事件推回发起请求的那条连接。
// 事件写失败(如客户端已断开)不应中断任务,调用方只记日志。
type EmitFunc func(event string, data any) error

// Router 把协议请求路由到具体能力实现,并持有连接池与活动任务表。
type Router struct {
	pool    *pool.Pool
	cfg     *config.File
	version string
	started time.Time

	// onTaskStart / onTaskEnd 供 daemon 统计活动任务(空闲退出判定用)。
	onTaskStart func()
	onTaskEnd   func()

	// baseCtx 是所有长任务(distribute)的父上下文:daemon 关闭时统一取消。
	baseCtx    context.Context
	baseCancel context.CancelFunc

	taskSeq atomic.Uint64

	mu    sync.Mutex
	tasks map[string]*taskEntry
}

// taskEntry 是一个活动任务。dist 非 nil 时为 distribute 任务(可查询逐机进度)。
type taskEntry struct {
	summary protocol.TaskSummary
	cancel  context.CancelFunc
}

// NewRouter 装配路由。cfg 用于把 group / 主机名解析成主机集合;version 用于 status。
func NewRouter(p *pool.Pool, cfg *config.File, version string) *Router {
	ctx, cancel := context.WithCancel(context.Background())
	return &Router{
		pool:       p,
		cfg:        cfg,
		version:    version,
		started:    time.Now(),
		baseCtx:    ctx,
		baseCancel: cancel,
		tasks:      map[string]*taskEntry{},
	}
}

// newPool 依据 hosts.yaml 构建连接池,并把加载到的配置一并返回给 Router
// (group 解析需要它)。配置缺失时返回空清单,让 daemon 仍能启动并由单条请求
// 报出「未知主机」,而不是整个进程崩掉。
func newPool(configPath string, logf func(string, ...any)) (*pool.Pool, *config.File) {
	if configPath == "" {
		configPath = config.ConfigPath()
	}
	f, err := config.Load(configPath)
	if err != nil {
		logf("daemon: 加载配置 %s 失败,将以空清单启动: %v", configPath, err)
		f = config.New()
	}
	return pool.New(f, pool.Options{
		OnWarn: func(format string, args ...any) { logf("daemon: pool: "+format, args...) },
	}), f
}

// Close 取消所有长任务并关闭连接池。
func (r *Router) Close() {
	r.baseCancel()
	r.mu.Lock()
	for _, t := range r.tasks {
		if t.cancel != nil {
			t.cancel()
		}
	}
	r.mu.Unlock()
	if r.pool != nil {
		r.pool.Close()
	}
}

// Handle 处理单条请求,始终返回一个可写回的响应(错误也转成响应)。
func (r *Router) Handle(ctx context.Context, req *protocol.Request, emit EmitFunc) *protocol.Response {
	resp, err := r.route(ctx, req, emit)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeInternal, "%v", err)
	}
	return resp
}

func (r *Router) route(ctx context.Context, req *protocol.Request, emit EmitFunc) (*protocol.Response, error) {
	switch req.Cmd {
	case protocol.CmdStatus:
		return protocol.OK(req.ID, r.status())
	case protocol.CmdExec:
		return r.exec(ctx, req)
	case protocol.CmdBatch:
		return r.batch(ctx, req, emit)
	default:
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "未知命令 %q", req.Cmd), nil
	}
}

// resolve 把 HostSel(显式主机 + 分组)解析成排序后的主机名列表。
func (r *Router) resolve(sel protocol.HostSel) ([]string, error) {
	if r.cfg == nil {
		return nil, fmt.Errorf("daemon: 未加载主机配置")
	}
	hs, err := r.cfg.Resolve(sel.Hosts, sel.Group)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.Name)
	}
	return names, nil
}

func (r *Router) status() *protocol.StatusResult {
	stats := r.pool.Stats()
	out := make([]protocol.PoolHost, 0, len(stats))
	for _, s := range stats {
		ph := protocol.PoolHost{
			Host:       s.Host,
			Connected:  s.Connected,
			Channels:   s.Channels,
			Reconnects: s.Reconnects,
		}
		if s.Connected {
			ph.DialedAt = s.DialedAt
		}
		out = append(out, ph)
	}

	r.mu.Lock()
	tasks := make([]protocol.TaskSummary, 0, len(r.tasks))
	for _, t := range r.tasks {
		tasks = append(tasks, t.summary)
	}
	r.mu.Unlock()

	return &protocol.StatusResult{
		Version: r.version,
		Uptime:  time.Since(r.started).Round(time.Second).String(),
		Pool:    out,
		Tasks:   tasks,
	}
}

func (r *Router) exec(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	var p protocol.ExecParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	if p.Host == "" || p.Command == "" {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "exec 需要 host 与 command"), nil
	}

	res, err := exec.Run(ctx, r.pool, p.Host, p.Command, p.Timeout, p.Shell)
	if err != nil {
		// 连接层失败:仍返回结构化结果,把 Err 置为该错误,便于客户端展示已收集到的
		// 部分输出(区分于命令自身的非零退出码)。
		out := protocol.ExecResult{Host: p.Host, Err: err.Error()}
		if res != nil {
			out.Stdout, out.Stderr, out.ExitCode = res.Stdout, res.Stderr, res.ExitCode
			out.Duration = res.Duration.String()
		}
		return protocol.OK(req.ID, out)
	}

	return protocol.OK(req.ID, protocol.ExecResult{
		Host:     res.Host,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		Duration: res.Duration.String(),
	})
}

func (r *Router) batch(ctx context.Context, req *protocol.Request, emit EmitFunc) (*protocol.Response, error) {
	var p protocol.BatchParams
	if err := req.Bind(&p); err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}
	if p.Command == "" {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "batch 需要 command"), nil
	}
	hosts, err := r.resolve(p.Sel)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}

	id := r.startTask("batch", len(hosts), nil)
	defer r.finishTask(id)

	out, err := exec.RunBatch(ctx, r.pool, hosts, p.Command, exec.BatchOptions{
		Concurrency: p.Concurrency,
		Timeout:     p.Timeout,
		Shell:       p.Shell,
	})
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "%v", err), nil
	}

	res := protocol.BatchResult{
		Total:   len(out.Results),
		Elapsed: out.Elapsed.String(),
		Results: make([]protocol.ExecResult, 0, len(out.Results)),
	}
	for _, er := range out.Results {
		pr := protocol.ExecResult{
			Host:     er.Host,
			Stdout:   er.Stdout,
			Stderr:   er.Stderr,
			ExitCode: er.ExitCode,
			Duration: er.Duration.String(),
		}
		if er.Err != nil {
			pr.Err = er.Err.Error()
		}
		if pr.Err == "" && pr.ExitCode == 0 {
			res.OK++
		} else {
			res.Failed++
		}
		res.Results = append(res.Results, pr)
		// 逐机完成事件:让 MCP / 交互客户端可以边跑边展示,而不必等整批结束。
		if emit != nil {
			_ = emit(protocol.EventProgress, pr)
		}
	}
	r.setTaskProgress(id, res.OK+res.Failed)
	return protocol.OK(req.ID, res)
}

// startTask 登记一个活动任务并返回其 id。cancel 非 nil 时随 daemon 关闭一并取消。
func (r *Router) startTask(kind string, total int, cancel context.CancelFunc) string {
	id := fmt.Sprintf("%s-%d", kind, r.taskSeq.Add(1))
	r.mu.Lock()
	r.tasks[id] = &taskEntry{
		summary: protocol.TaskSummary{ID: id, Kind: kind, Total: total},
		cancel:  cancel,
	}
	r.mu.Unlock()
	if r.onTaskStart != nil {
		r.onTaskStart()
	}
	return id
}

// setTaskProgress 更新任务的完成数(供 status 展示)。
func (r *Router) setTaskProgress(id string, done int) {
	r.mu.Lock()
	if t := r.tasks[id]; t != nil {
		t.summary.Done = done
	}
	r.mu.Unlock()
}

// finishTask 移除任务并递减活动计数。
func (r *Router) finishTask(id string) {
	r.mu.Lock()
	_, ok := r.tasks[id]
	delete(r.tasks, id)
	r.mu.Unlock()
	if ok && r.onTaskEnd != nil {
		r.onTaskEnd()
	}
}
