package main

import "github.com/spf13/cobra"

func newDistributeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "distribute <local> <host...|--group> <remote>",
		Short: "单文件向多机并发分发(磁盘只读一次,慢机不阻塞快机)",
		Args:  cobra.MinimumNArgs(3),
		RunE:  notImplemented("distribute"),
	}
	cmd.Flags().String("group", "", "目标分组")
	cmd.Flags().Int("concurrency", 16, "并发度")
	cmd.Flags().Bool("resume", false, "对失败机器从上次 offset 续传")
	return cmd
}

func newUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <host> <local> <remote>",
		Short: "SFTP 单机上传(带进度)",
		Args:  cobra.ExactArgs(3),
		RunE:  notImplemented("upload"),
	}
}

func newDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <host> <remote> <local>",
		Short: "SFTP 单机下载(带进度)",
		Args:  cobra.ExactArgs(3),
		RunE:  notImplemented("download"),
	}
}
