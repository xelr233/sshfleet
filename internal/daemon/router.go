package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/exec"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/protocol"
)

// Router 把协议请求路由到具体能力实现,并持有连接池。
type Router struct {
	pool    *pool.Pool
	version string
	started time.Time

	// onTaskStart / onTaskEnd 供 daemon 统计活动任务(空闲退出判定用)。
	onTaskStart func()
	onTaskEnd   func()

	mu    sync.Mutex
	tasks map[string]*protocol.TaskSummary
}

// NewRouter 装配路由。version 用于 status 输出。
func NewRouter(p *pool.Pool, version string) *Router {
	return &Router{
		pool:    p,
		version: version,
		started: time.Now(),
		tasks:   map[string]*protocol.TaskSummary{},
	}
}

// newPool 依据 hosts.yaml 构建连接池。配置缺失时返回仍可工作的空池,
// 这样 daemon 能正常启动、由单条请求报出「未知主机」而不是整个进程崩掉。
func newPool(configPath string, logf func(string, ...any)) *pool.Pool {
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
	})
}

// Close 关闭连接池。
func (r *Router) Close() {
	if r.pool != nil {
		r.pool.Close()
	}
}

// Handle 处理单条请求,始终返回一个可写回的响应(错误也转成响应)。
func (r *Router) Handle(ctx context.Context, req *protocol.Request) *protocol.Response {
	resp, err := r.route(ctx, req)
	if err != nil {
		return protocol.Fail(req.ID, protocol.CodeInternal, "%v", err)
	}
	return resp
}

func (r *Router) route(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	switch req.Cmd {
	case protocol.CmdStatus:
		return protocol.OK(req.ID, r.status())
	case protocol.CmdExec:
		return r.exec(ctx, req)
	default:
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "未知命令 %q", req.Cmd), nil
	}
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
		tasks = append(tasks, *t)
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

// taskDone 标记任务完成并从活动表移除(供 M2/M3 的 batch/distribute 复用)。
func (r *Router) taskDone(id string) {
	r.mu.Lock()
	delete(r.tasks, id)
	r.mu.Unlock()
	if r.onTaskEnd != nil {
		r.onTaskEnd()
	}
}
