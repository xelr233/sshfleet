package main

import "github.com/spf13/cobra"

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "长连接 shell 会话(cd / 环境变量延续)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "open <host>",
			Short: "打开会话,返回 session_id",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("session open"),
		},
		&cobra.Command{
			Use:   "send <id> <input>",
			Short: "向会话写入输入,回输出",
			Args:  cobra.ExactArgs(2),
			RunE:  notImplemented("session send"),
		},
		&cobra.Command{
			Use:   "close <id>",
			Short: "关闭会话",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("session close"),
		},
		&cobra.Command{
			Use:   "list",
			Short: "列出活动会话",
			RunE:  notImplemented("session list"),
		},
	)
	return cmd
}

func newTailCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tail <host> <file>",
		Short: "流式跟随远端文件(tail -f 类,断线重连不丢事件)",
		Args:  cobra.ExactArgs(2),
		RunE:  notImplemented("tail"),
	}
}
