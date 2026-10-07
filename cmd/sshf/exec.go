package main

import "github.com/spf13/cobra"

func newExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec <host> <cmd>",
		Short: "单机执行命令,回 stdout/stderr/exit code",
		Args:  cobra.ExactArgs(2),
		RunE:  notImplemented("exec", "M1"),
	}
	cmd.Flags().Duration("timeout", 0, "单机超时(0 = 默认)")
	return cmd
}

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
