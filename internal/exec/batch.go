package exec

import (
	"context"
	"fmt"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/xelr233/sshfleet/internal/pool"
)

// DefaultConcurrency 是 batch 未指定并发度时的默认值(与 windvalley/gossh 一致)。
const DefaultConcurrency = 5

// BatchOptions 控制多机并发执行。
type BatchOptions struct {
	// Concurrency 是同时在执行的主机数上限;<=0 用 DefaultConcurrency。
	Concurrency int
	// Timeout 是单机超时(每台从各自开始执行时独立计时)。
	Timeout time.Duration
	// Shell 覆盖各主机的 shell 设定(空 = 各用自己主机配置里的值)。
	Shell string
}

// BatchOutcome 是 batch 的聚合结果。
type BatchOutcome struct {
	// Results 与输入主机列表按名排序后一一对应。
	Results []Result
	Elapsed time.Duration
}

// RunBatch 在 hosts 上并发执行同一命令,逐机独立超时,结果按主机名排序。
//
// 单机失败(连接层错误或非零退出码)不中断其它机器:错误记录在各机 Result 内,
// RunBatch 只对「参数非法」返回 error。ctx 取消会中止所有未完成的机器。
//
// 并发度由 semaphore 限制(README §5.3/§8):不会为每台机器各起一条不受控的
// 连接风暴,而是最多 Concurrency 条在内。
func RunBatch(ctx context.Context, p *pool.Pool, hosts []string, command string, opts BatchOptions) (*BatchOutcome, error) {
	if p == nil {
		return nil, fmt.Errorf("batch: 连接池为空")
	}
	if command == "" {
		return nil, fmt.Errorf("batch: 命令不能为空")
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("batch: 未指定主机")
	}

	ordered := append([]string(nil), hosts...)
	sort.Strings(ordered)

	conc := opts.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	if conc > len(ordered) {
		conc = len(ordered)
	}

	start := time.Now()
	results := make([]Result, len(ordered))
	sem := semaphore.NewWeighted(int64(conc))
	g, gctx := errgroup.WithContext(ctx)

	// i 与 h 在 Go 1.22+ 是每轮独立变量,可被 goroutine 安全捕获;
	// results 的每个下标只被一个 goroutine 写,无数据竞争。
	for i, h := range ordered {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if err := sem.Acquire(gctx, 1); err != nil {
				return err // 只有 ctx 取消会走到这里
			}
			defer sem.Release(1)

			res, err := Run(gctx, p, h, command, opts.Timeout, opts.Shell)
			if res == nil {
				res = &Result{Host: h}
			}
			if err != nil {
				res.Err = err
			}
			results[i] = *res
			return nil
		})
	}

	waitErr := g.Wait()
	if waitErr != nil && gctx.Err() == nil {
		return nil, waitErr
	}

	// 因 ctx 取消而没轮到的机器补成明确错误,避免结果里出现空白主机。
	for i := range results {
		if results[i].Host == "" {
			results[i] = Result{Host: ordered[i], Err: ctx.Err()}
		}
	}
	return &BatchOutcome{Results: results, Elapsed: time.Since(start)}, nil
}
