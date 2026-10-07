package main

import "github.com/spf13/cobra"

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "启动 sshfleet daemon(监听 127.0.0.1,写 token 文件)",
		RunE:  notImplemented("start"),
	}
	cmd.Flags().Bool("foreground", false, "前台运行,不 detach")
	return cmd
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "停止 daemon",
		RunE:  notImplemented("stop"),
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "查看 daemon 版本、连接池状态、活动任务",
		RunE:  notImplemented("status"),
	}
}
