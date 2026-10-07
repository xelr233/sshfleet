package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/protocol"
)

func newUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <host> <local> <remote>",
		Short: "SFTP 单机上传(带进度)",
		Args:  exactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSFTPTransfer(cmd, protocol.CmdSFTPPut, args[0], args[1], args[2])
		},
	}
}

func newDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <host> <remote> <local>",
		Short: "SFTP 单机下载(带进度)",
		Args:  exactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSFTPTransfer(cmd, protocol.CmdSFTPGet, args[0], args[2], args[1])
		},
	}
}

// runSFTPTransfer 走协议完成一次单机传输,进度事件在 stderr 原地刷新。
func runSFTPTransfer(cmd *cobra.Command, command protocol.Command, host, local, remote string) error {
	c, err := ensureDaemon(cmd)
	if err != nil {
		return err
	}
	defer c.Close()

	lastPct := -1
	resp, err := c.DoEvents(cmd.Context(), command, protocol.SFTPParams{
		Host: host, Local: local, Remote: remote,
	}, func(ev *protocol.Event) {
		if ev.Event != protocol.EventProgress {
			return
		}
		var p protocol.SFTPProgress
		if ev.Bind(&p) != nil {
			return
		}
		pct := percent(p.Bytes, p.Total)
		if pct == lastPct {
			return
		}
		lastPct = pct
		fmt.Fprintf(cmd.ErrOrStderr(), "\r%s → %s  %s  %d%%",
			host, remote, humanBytes(p.Bytes), pct)
	})
	if err != nil {
		return err
	}
	if err := resp.Err(); err != nil {
		return err
	}
	var res protocol.SFTPResult
	if err := resp.Bind(&res); err != nil {
		return err
	}
	if lastPct >= 0 {
		fmt.Fprintln(cmd.ErrOrStderr())
	}
	if res.Err != "" {
		return fmt.Errorf("sftp %s: %s", host, res.Err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: 传输 %s,耗时 %s\n", host, humanBytes(res.Bytes), res.Duration)
	return nil
}
