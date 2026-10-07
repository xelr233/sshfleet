package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/protocol"
)

func newExecCmd() *cobra.Command {
	var (
		timeout time.Duration
		shell   string
	)
	cmd := &cobra.Command{
		Use:   "exec <host> <cmd>",
		Short: "单机执行命令,回 stdout/stderr/exit code",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			ctx := cmd.Context()
			// 给「连接 + 执行」留出总时长上限,避免 daemon 卡死时 CLI 无限等待。
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout+30*time.Second)
				defer cancel()
			}

			resp, err := c.Do(ctx, protocol.CmdExec, protocol.ExecParams{
				Host:    args[0],
				Command: args[1],
				Timeout: timeout,
				Shell:   shell,
			})
			if err != nil {
				return err
			}
			if err := resp.Err(); err != nil {
				return err
			}
			var res protocol.ExecResult
			if err := resp.Bind(&res); err != nil {
				return err
			}

			// stdout/stderr 原样透传,便于管道组合使用。
			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()
			if res.Stdout != "" {
				fmt.Fprint(out, res.Stdout)
				if !strings.HasSuffix(res.Stdout, "\n") {
					fmt.Fprintln(out)
				}
			}
			if res.Stderr != "" {
				fmt.Fprint(errOut, res.Stderr)
				if !strings.HasSuffix(res.Stderr, "\n") {
					fmt.Fprintln(errOut)
				}
			}

			// 连接层错误(区别于命令非零退出码)。
			if res.Err != "" {
				return fmt.Errorf("exec %s: %s", res.Host, res.Err)
			}
			// 用命令的退出码作为 CLI 退出码,便于脚本判断。
			if res.ExitCode != 0 {
				return &exitCodeError{code: res.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().DurationVarP(&timeout, "timeout", "t", 0, "单机超时(0 = 无超时)")
	cmd.Flags().StringVar(&shell, "shell", "", "覆盖主机的 shell 设定(auto/bash/sh/cmd/powershell/pwsh)")
	return cmd
}

// exitCodeError 携带远端命令的退出码,由 main 转成进程退出码。
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("远端退出码 %d", e.code) }

func newBatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "batch <cmd>",
		Short: "多机并发执行,输出逐机块 + 汇总统计",
		Args:  cobra.ExactArgs(1),
		RunE:  notImplemented("batch", "M2"),
	}
	cmd.Flags().StringSlice("host", nil, "目标主机(可多次)")
	cmd.Flags().String("group", "", "目标分组(hosts.yaml 中的 group)")
	cmd.Flags().IntP("concurrency", "c", 5, "并发度")
	cmd.Flags().DurationP("timeout", "t", 0, "单机超时")
	return cmd
}
