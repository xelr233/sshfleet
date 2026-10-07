//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detach 让 daemon 子进程脱离当前控制台:新建进程组且不继承控制台,
// 这样父 CLI 退出后 daemon 仍存活。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}

// terminate 结束进程。Windows 无优雅 SIGTERM,用 taskkill(不带 /F 先尝试关闭)。
func terminate(pid int) error {
	out, err := exec.Command("taskkill", "/PID", fmt.Sprint(pid), "/T", "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}
