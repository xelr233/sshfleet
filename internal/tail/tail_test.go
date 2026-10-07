package tail

import (
	"sync"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

func newTailPool(t *testing.T, interval time.Duration) (*pool.Pool, *sshtest.Server) {
	t.Helper()
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatal(err)
	}
	srv.TailInterval = interval
	t.Cleanup(srv.Close)
	cfg := config.New()
	_ = cfg.Add(&config.Host{Name: "h1", Addr: srv.Host(), Port: srv.Port(), User: sshtest.User, Password: sshtest.Password})
	p := pool.New(cfg, pool.Options{})
	t.Cleanup(func() { p.Close() })
	return p, srv
}

// 收集 seq > since 的行,直到达到 n 行或超时。
func collectLines(t *testing.T, task *Task, since int64, n int, timeout time.Duration) []Line {
	t.Helper()
	var mu sync.Mutex
	var got []Line
	ch := make(chan Line, 1024)
	cancel := task.Subscribe(since, func(ln Line) {
		select {
		case ch <- ln:
		default:
		}
	})
	defer cancel()

	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case ln := <-ch:
			mu.Lock()
			got = append(got, ln)
			mu.Unlock()
		case <-deadline:
			mu.Lock()
			defer mu.Unlock()
			return got
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return got
}

// 基本跟随:持续收到带连续 seq 的行。
func TestTailStreamsLines(t *testing.T) {
	p, _ := newTailPool(t, 20*time.Millisecond)
	m := NewManager()
	task, err := m.Start(p, "h1", "/var/log/x.log")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.StopAll)

	lines := collectLines(t, task, 0, 3, 3*time.Second)
	if len(lines) < 3 {
		t.Fatalf("应至少收到 3 行, got %d", len(lines))
	}
	for i, ln := range lines {
		if ln.Seq != int64(i+1) {
			t.Fatalf("第 %d 行 seq 应为 %d, got %d", i, i+1, ln.Seq)
		}
	}
}

// §9 M4 验收:客户端断开重连不丢已推事件。
// 用「游标重订阅」模拟断线重连:新订阅带 since=已收到的最大 seq,
// 应从环形缓冲补回这期间产生的行,序号连续无缺口。
func TestTailReconnectReplaysFromRingBuffer(t *testing.T) {
	p, _ := newTailPool(t, 10*time.Millisecond)
	m := NewManager()
	task, err := m.Start(p, "h1", "/var/log/x.log")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.StopAll)

	// 第一次订阅:收几行后「断开」(取消订阅)。
	first := collectLines(t, task, 0, 3, 3*time.Second)
	if len(first) < 3 {
		t.Fatalf("首次应收到 3 行, got %d", len(first))
	}
	lastSeq := first[len(first)-1].Seq

	// 断开期间任务继续产生行。
	time.Sleep(150 * time.Millisecond)

	// 「重连」:带 since=lastSeq 再订阅,应补回缺口并继续。
	replay := collectLines(t, task, lastSeq, 3, 3*time.Second)
	if len(replay) < 3 {
		t.Fatalf("重连应至少补回 3 行, got %d", len(replay))
	}
	// 序号必须严格连续:重连不丢事件,且不重复。
	want := lastSeq
	for i, ln := range replay {
		want++
		if ln.Seq != want {
			t.Fatalf("重连第 %d 行 seq 应为 %d, got %d(有丢/重)", i, want, ln.Seq)
		}
	}
}

// 环形缓冲有界:只保留最近 DefaultBufferLines 行。
func TestTailRingBufferBounded(t *testing.T) {
	task := &Task{done: make(chan struct{})}
	for i := 0; i < DefaultBufferLines+500; i++ {
		task.append("x")
	}
	task.mu.Lock()
	n := len(task.lines)
	task.mu.Unlock()
	if n != DefaultBufferLines {
		t.Fatalf("环形缓冲应保留 %d 行, got %d", DefaultBufferLines, n)
	}
}

// 多客户端订阅同一任务:都收到同样的行。
func TestTailMultipleSubscribers(t *testing.T) {
	p, _ := newTailPool(t, 20*time.Millisecond)
	m := NewManager()
	task, err := m.Start(p, "h1", "/var/log/x.log")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.StopAll)

	var wg sync.WaitGroup
	results := make([][]Line, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = collectLines(t, task, 0, 2, 3*time.Second)
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if len(r) < 2 {
			t.Fatalf("订阅者 %d 应收到 2 行, got %d", i, len(r))
		}
	}
	if results[0][0].Seq != results[1][0].Seq {
		t.Fatalf("两个订阅者首行 seq 应一致: %d vs %d", results[0][0].Seq, results[1][0].Seq)
	}
}

func TestTailStopRemovesTask(t *testing.T) {
	p, _ := newTailPool(t, 20*time.Millisecond)
	m := NewManager()
	task, err := m.Start(p, "h1", "/x")
	if err != nil {
		t.Fatal(err)
	}
	id := task.ID()
	if got := m.List(); len(got) != 1 {
		t.Fatalf("应有 1 个任务, got %d", len(got))
	}
	if err := m.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := m.List(); len(got) != 0 {
		t.Fatalf("停止后应无任务, got %d", len(got))
	}
	if err := m.Stop(id); err == nil {
		t.Fatal("重复停止应报错")
	}
}
