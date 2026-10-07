//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detach 让 daemon 子进程脱离当前会话,父进程退出后仍存活。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminate 请求某进程退出(SIGTERM;不强制 kill,留给 daemon 清理运行时文件)。
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}
