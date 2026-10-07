package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/daemon"
	"github.com/xelr233/sshfleet/internal/protocol"
)

// startTimeout 是 CLI 等待新 daemon 就绪的上限。
const startTimeout = 10 * time.Second

func newStartCmd() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "启动 sshfleet daemon(监听 127.0.0.1,写 token 文件)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if daemon.IsRunning() {
				return fmt.Errorf("daemon 已在运行(PID %d);如需重启请先 `sshf stop`", mustPID())
			}
			if foreground {
				return runDaemonForeground(cmd)
			}
			return spawnDaemon(cmd)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "前台运行,不 detach")
	return cmd
}

// runDaemonForeground 在当前进程内运行 daemon,直到 ctx 取消。
func runDaemonForeground(cmd *cobra.Command) error {
	d := daemon.New(daemon.Options{
		ConfigPath: configPath,
		Version:    version,
		Logf:       func(format string, args ...any) { fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...) },
	})
	if err := d.Listen(); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "daemon 前台运行中,监听 %s\n", d.Addr())
	return d.Serve(cmd.Context())
}

// spawnDaemon 以分离子进程方式拉起 daemon,并等待其就绪。
func spawnDaemon(cmd *cobra.Command) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件: %w", err)
	}
	args := []string{"start", "--foreground"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	child := exec.Command(exe, args...)
	child.Stdout = nil
	child.Stderr = nil
	detach(child)

	if err := child.Start(); err != nil {
		return fmt.Errorf("启动 daemon 子进程: %w", err)
	}
	// 不 Wait:子进程已分离,由 PID 文件与外层 stop 管理。
	_ = child.Process.Release()

	return waitDaemonReady(cmd)
}

// waitDaemonReady 轮询端口文件,直到 daemon 可连接或超时。
func waitDaemonReady(cmd *cobra.Command) error {
	deadline := time.Now().Add(startTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		port, err := daemon.ReadPort()
		if err != nil {
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		token, err := daemon.ReadToken()
		if err != nil {
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if c, err := daemon.DialAddr(port, token); err == nil {
			c.Close()
			fmt.Fprintf(cmd.OutOrStdout(), "daemon 已启动,监听 127.0.0.1:%d\n", port)
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("daemon 启动超时(%s): %v;可用 `sshf start --foreground` 查看日志", startTimeout, lastErr)
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "停止 daemon",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := daemon.ReadPID()
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon 未在运行")
				return nil
			}
			if err := terminate(pid); err != nil {
				return fmt.Errorf("终止 daemon(PID %d): %w", pid, err)
			}
			// 等进程真正退出,再清理遗留文件。
			for i := 0; i < 50; i++ {
				if !daemon.IsRunning() {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			daemon.ClearRuntimeState()
			fmt.Fprintf(cmd.OutOrStdout(), "daemon 已停止(PID %d)\n", pid)
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "查看 daemon 版本、连接池状态、活动任务",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			resp, err := c.Do(ctx, protocol.CmdStatus, nil)
			if err != nil {
				return err
			}
			if err := resp.Err(); err != nil {
				return err
			}
			var st protocol.StatusResult
			if err := resp.Bind(&st); err != nil {
				return err
			}
			printStatus(cmd, &st)
			return nil
		},
	}
}

// ensureDaemon 返回一个已连接的客户端:daemon 未运行时自动拉起。
func ensureDaemon(cmd *cobra.Command) (*daemon.Client, error) {
	if c, err := daemon.Dial(); err == nil {
		return c, nil
	}
	if err := spawnDaemon(cmd); err != nil {
		return nil, err
	}
	return daemon.Dial()
}

func mustPID() int {
	pid, _ := daemon.ReadPID()
	return pid
}
