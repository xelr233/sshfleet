package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "sshf",
		Short:   "sshfleet: SSH 多机管理(daemon + 连接池 + 批量执行/分发)",
		Version: version,
		Long: `sshfleet CLI。

CLI 是无状态薄客户端:连接本机 daemon(未运行时自动拉起,或 --no-daemon 单次直连),
所有 SSH 逻辑都在 daemon 中,CLI 与 daemon 之间走 127.0.0.1 TCP + token 的 JSON 行协议。`,
		SilenceUsage: true,
	}
	root.AddCommand(
		newStartCmd(),
		newStopCmd(),
		newStatusCmd(),
		newHostsCmd(),
		newExecCmd(),
		newBatchCmd(),
		newDistributeCmd(),
		newUploadCmd(),
		newDownloadCmd(),
		newSessionCmd(),
		newTailCmd(),
		newMCPCmd(),
	)
	return root
}

// notImplemented 是 M0 骨架阶段各子命令的占位执行体。
func notImplemented(name string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("%s: 尚未实现", name)
	}
}
