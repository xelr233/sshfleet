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
	var (
		hosts       []string
		group       string
		concurrency int
		timeout     time.Duration
		shell       string
	)
	cmd := &cobra.Command{
		Use:   "batch [--host h]... [--group g] <cmd>",
		Short: "多机并发执行,输出逐机块 + 汇总统计",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(hosts) == 0 && group == "" {
				return fmt.Errorf("batch: 需指定 --host 或 --group")
			}
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			// 单个客户端连接:先收到逐机进度事件(边跑边打),再收到汇总响应。
			resp, err := c.DoEvents(cmd.Context(), protocol.CmdBatch, protocol.BatchParams{
				Sel:         protocol.HostSel{Hosts: hosts, Group: group},
				Command:     args[0],
				Concurrency: concurrency,
				Timeout:     timeout,
				Shell:       shell,
			}, func(ev *protocol.Event) {
				if ev.Event != protocol.EventProgress {
					return
				}
				var pr protocol.ExecResult
				if err := ev.Bind(&pr); err != nil {
					return
				}
				printExecBlock(cmd, &pr)
			})
			if err != nil {
				return err
			}
			if err := resp.Err(); err != nil {
				return err
			}
			var res protocol.BatchResult
			if err := resp.Bind(&res); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprint(out, "\n=== 汇总 ===\n")
			fmt.Fprintf(out, "总计 %d 台,成功 %d,失败 %d,耗时 %s\n", res.Total, res.OK, res.Failed, res.Elapsed)
			if res.Failed > 0 {
				fmt.Fprintf(out, "失败机器: %s\n", strings.Join(failedHosts(res.Results), ", "))
				return &exitCodeError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&hosts, "host", nil, "目标主机(可多次 / 逗号分隔)")
	cmd.Flags().StringVar(&group, "group", "", "目标分组(hosts.yaml 中的 group)")
	cmd.Flags().IntVarP(&concurrency, "concurrency", "c", 0, "并发度(0 = 默认 5)")
	cmd.Flags().DurationVarP(&timeout, "timeout", "t", 0, "单机超时(0 = 无超时)")
	cmd.Flags().StringVar(&shell, "shell", "", "覆盖主机的 shell 设定")
	return cmd
}

// printExecBlock 打印一台机器的执行块(=== host === 风格,便于人读与机器解析)。
func printExecBlock(cmd *cobra.Command, r *protocol.ExecResult) {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(out, "=== %s ===\n", r.Host)
	if r.Stdout != "" {
		fmt.Fprint(out, r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			fmt.Fprintln(out)
		}
	}
	if r.Stderr != "" {
		fmt.Fprint(errOut, r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			fmt.Fprintln(errOut)
		}
	}
	if r.Err != "" {
		fmt.Fprintf(errOut, "[连接错误] %s\n", r.Err)
		return
	}
	if r.ExitCode != 0 {
		fmt.Fprintf(out, "[退出码 %d]\n", r.ExitCode)
	}
}

// failedHosts 从结果中挑出失败(连接错误或非零退出码)的主机名。
func failedHosts(results []protocol.ExecResult) []string {
	var out []string
	for _, r := range results {
		if r.Err != "" || r.ExitCode != 0 {
			out = append(out, r.Host)
		}
	}
	return out
}
