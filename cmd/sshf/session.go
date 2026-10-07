package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/daemon"
	"github.com/xelr233/sshfleet/internal/protocol"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "长连接 shell 会话(cd / 环境变量延续)",
	}
	cmd.AddCommand(
		newSessionOpenCmd(),
		newSessionSendCmd(),
		newSessionCloseCmd(),
		newSessionListCmd(),
	)
	return cmd
}

func newSessionOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <host>",
		Short: "打开会话,返回 session_id(会话存活于 daemon)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			resp, err := doReq(c, cmd, protocol.CmdSessionOpen, protocol.SessionOpenParams{Host: args[0]})
			if err != nil {
				return err
			}
			var res protocol.SessionOpenResult
			if err := resp.Bind(&res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", res.SessionID)
			return nil
		},
	}
}

func newSessionSendCmd() *cobra.Command {
	var newline bool
	cmd := &cobra.Command{
		Use:   "send <id> <input>",
		Short: "向会话写入输入,回本次输出",
		Args:  minArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := strings.Join(args[1:], " ")
			if newline {
				input += "\n"
			}
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			resp, err := doReq(c, cmd, protocol.CmdSessionSend, protocol.SessionSendParams{
				SessionID: args[0], Input: input,
			})
			if err != nil {
				return err
			}
			var out protocol.SessionOutput
			if err := resp.Bind(&out); err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out.Data)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&newline, "newline", "n", true, "在输入末尾追加换行(回车)")
	return cmd
}

func newSessionCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "close <id>",
		Short: "关闭会话",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()
			if _, err := doReq(c, cmd, protocol.CmdSessionClose, protocol.SessionCloseParams{SessionID: args[0]}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "会话 %s 已关闭\n", args[0])
			return nil
		},
	}
}

func newSessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出活动会话",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			resp, err := doReq(c, cmd, protocol.CmdSessionList, nil)
			if err != nil {
				return err
			}
			var res protocol.SessionListResult
			if err := resp.Bind(&res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Sessions) == 0 {
				fmt.Fprintln(out, "无活动会话")
				return nil
			}
			for _, s := range res.Sessions {
				fmt.Fprintf(out, "%s\t%s\t已开 %s\t空闲 %s\n",
					s.SessionID, s.Host, s.OpenedAt.Format("15:04:05"), s.Idle)
			}
			return nil
		},
	}
}

func newTailCmd() *cobra.Command {
	var (
		taskID     string
		since      int64
		maxSeconds int
	)
	cmd := &cobra.Command{
		Use:   "tail <host> <file>",
		Short: "流式跟随远端文件(断线重连不丢事件:--task --since)",
		Long: `跟随远端文件的新增行(远端跑 tail -f)。

首次跟随:sshf tail <host> <file>,记下输出的 #seq。
断线后重连:sshf tail --task <task_id> --since <最后收到的 seq>,
从环形缓冲补回缺失的行,不丢事件。`,
		Args: rangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if taskID == "" && len(args) != 2 {
				return fmt.Errorf("tail: 首次跟随需 <host> <file>,重连需 --task <id>")
			}
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			ctx := cmd.Context()
			if maxSeconds > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(maxSeconds)*time.Second)
				defer cancel()
			}

			params := protocol.TailStartParams{TaskID: taskID, Since: since}
			if taskID == "" {
				params.Host, params.Path = args[0], args[1]
			}

			out := cmd.OutOrStdout()
			err = c.Stream(ctx, protocol.CmdTailStart, params,
				func(ev *protocol.Event) {
					if ev.Event != protocol.EventTailLine {
						return
					}
					var ln protocol.TailLine
					if err := ev.Bind(&ln); err != nil {
						return
					}
					// 带 seq 输出,便于重连时作为 --since。
					fmt.Fprintf(out, "%6d  %s\n", ln.Seq, ln.Line)
				},
				func(resp *protocol.Response) {
					if resp.Err() != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tail 错误: %v\n", resp.Err())
						return
					}
					var res protocol.TailStartResult
					if err := resp.Bind(&res); err == nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "# task_id=%s(重连用 --task %s --since <seq>)\n", res.TaskID, res.TaskID)
					}
				})
			if err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&taskID, "task", "", "重连到已有 tail 任务")
	cmd.Flags().Int64Var(&since, "since", 0, "只回放 seq 大于该值的缓冲行(配合 --task)")
	cmd.Flags().IntVar(&maxSeconds, "for", 0, "最长跟随秒数(0 = 直到 Ctrl-C)")
	return cmd
}

// doReq 发送一条请求并解析响应(通用错误处理)。
func doReq(c *daemon.Client, cmd *cobra.Command, command protocol.Command, params any) (*protocol.Response, error) {
	resp, err := c.Do(cmd.Context(), command, params)
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return resp, nil
}
