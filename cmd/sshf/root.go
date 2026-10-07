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

CLI 是无状态薄客户端:连接本机 daemon(未运行时自动拉起),
所有 SSH 逻辑都在 daemon 中,CLI 与 daemon 之间走 127.0.0.1 TCP + token 的 JSON 行协议。`,
		SilenceUsage:  true,
		SilenceErrors: true, // 错误由 main 统一打印,避免与退出码处理重复输出
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "hosts.yaml 路径(默认 ~/.sshfleet/hosts.yaml)")
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
	// 覆盖 cobra 自动生成的英文界面(用法/帮助/补全/默认标志)。
	localize(root)
	return root
}

// notImplemented 是尚未落地命令的占位执行体。
// name 是命令名,milestone 是计划实现的里程碑(README §9),便于 grep 追踪进度。
func notImplemented(name, milestone string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("TODO(%s): %s 尚未实现(见 README §9)", milestone, name)
	}
}
