package main

import "github.com/spf13/cobra"

func newUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <host> <local> <remote>",
		Short: "SFTP 单机上传(带进度)",
		Args:  cobra.ExactArgs(3),
		RunE:  notImplemented("upload", "M6"),
	}
}

func newDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <host> <remote> <local>",
		Short: "SFTP 单机下载(带进度)",
		Args:  cobra.ExactArgs(3),
		RunE:  notImplemented("download", "M6"),
	}
}
