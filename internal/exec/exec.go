package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/xelr233/sshfleet/internal/encoding"
	"github.com/xelr233/sshfleet/internal/pool"
)

// Result 是单机执行结果。
type Result struct {
	Host     string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	// Err 仅表示连接 / 通道层错误(如断开、无法建会话),与命令自身的非零退出码区分。
	Err error
}

// Run 在 host 上执行 command,返回其输出与退出码。
//
// command 的包装方式取决于主机的 shell 设定(默认原样交给登录 shell,
// 见 BuildInvocation)。执行占用一个 channel 名额,受 ctx 与 timeout 约束。
//
// shellOverride 非空时覆盖主机配置里的 shell 设定(对应 --shell 标志)。
func Run(ctx context.Context, p *pool.Pool, hostName, command string, timeout time.Duration, shellOverride string) (*Result, error) {
	conn, err := p.Get(ctx, hostName)
	if err != nil {
		return nil, err
	}

	shell := shellOverride
	if shell == "" {
		shell = conn.Host().Shell
	}
	inv, err := BuildInvocation(shell, command)
	if err != nil {
		return nil, err
	}

	// 单机超时覆盖 ctx。
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := conn.Acquire(ctx); err != nil {
		return nil, err
	}
	defer conn.Release()

	res := &Result{Host: hostName}
	start := time.Now()
	err = runOnConn(ctx, conn.Client(), inv, res)
	res.Duration = time.Since(start)

	// 远端可能是 Windows(GBK 输出):按 UTF-8 优先、GBK 兜底解码(README §5.9)。
	res.Stdout = encoding.ToUTF8([]byte(res.Stdout))
	res.Stderr = encoding.ToUTF8([]byte(res.Stderr))

	if err != nil {
		// 连接层错误:交给调用方决定是否重试;连接本身由 pool 的 watch 判定失效。
		res.Err = err
		return res, err
	}
	return res, nil
}

// runOnConn 在一条已取得的连接上跑一次 exec 会话,填充 res 的输出与退出码。
func runOnConn(ctx context.Context, client *ssh.Client, inv Invocation, res *Result) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("exec: 建立会话: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if inv.Stdin != "" {
		session.Stdin = bytes.NewBufferString(inv.Stdin)
	}

	// 用独立 goroutine 在 ctx 取消时关会话,解除阻塞中的 Wait。
	// 不能用 session.Signal:远端 shell 可能不实现 signal 请求,关会话更可靠。
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-done:
		}
	}()

	// Wait 内部先启动会话再等待退出。对需要 stdin 的脚本,Wait 会等 stdin 读完。
	err = session.Run(inv.Command)
	close(done)

	res.Stdout = stdout.String()
	res.Stderr = stderr.String()

	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			// 命令跑完了,只是退出码非零 —— 不是连接层错误。
			res.ExitCode = exitErr.ExitStatus()
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("exec: 执行超时或被取消: %w", ctx.Err())
		}
		return fmt.Errorf("exec: 运行命令: %w", err)
	}
	res.ExitCode = 0
	return nil
}
