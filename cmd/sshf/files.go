package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/protocol"
)

func newUploadCmd() *cobra.Command {
	var allowWindowsRemote bool
	cmd := &cobra.Command{
		Use:   "upload <host> <local> <remote>",
		Short: "SFTP 单机上传(带进度)",
		Long:  "把本地文件上传到远端。\n\n远端路径请写 POSIX 形式(如 /tmp/a.zip)。在 Git Bash 里,形如 /tmp/a.zip 的参数\n会被 MSYS2 自动改写成 C:/Users/... 这类本地路径,sshf 收到的已不是你想写的值;\n此时命令会报错并给出绕过办法(MSYS2_ARG_CONV_EXCL='*')。目标确实是 Windows\n主机时,用 --allow-windows-remote 按原样发送 Windows 路径。",
		Args:  exactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := guardRemotePath("upload", args[1], args[2], allowWindowsRemote); err != nil {
				return err
			}
			return runSFTPTransfer(cmd, protocol.CmdSFTPPut, args[0], args[1], args[2])
		},
	}
	cmd.Flags().BoolVar(&allowWindowsRemote, "allow-windows-remote", false,
		"允许远端路径为 Windows 形式(目标为 Windows 主机时使用)")
	return cmd
}

func newDownloadCmd() *cobra.Command {
	var allowWindowsRemote bool
	cmd := &cobra.Command{
		Use:   "download <host> <remote> <local>",
		Short: "SFTP 单机下载(带进度)",
		Long:  "把远端文件下载到本地。\n\n远端路径请写 POSIX 形式(如 /var/log/app.log)。在 Git Bash 里,形如 /tmp/a.zip\n的参数会被 MSYS2 自动改写成 C:/Users/... 这类本地路径;此时命令会报错并给出\n绕过办法。目标确实是 Windows 主机时,用 --allow-windows-remote 按原样发送。",
		Args:  exactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := guardRemotePath("download", args[2], args[1], allowWindowsRemote); err != nil {
				return err
			}
			return runSFTPTransfer(cmd, protocol.CmdSFTPGet, args[0], args[2], args[1])
		},
	}
	cmd.Flags().BoolVar(&allowWindowsRemote, "allow-windows-remote", false,
		"允许远端路径为 Windows 形式(目标为 Windows 主机时使用)")
	return cmd
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
