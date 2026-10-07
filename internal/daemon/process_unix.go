//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// processAlive 用信号 0 探测进程是否存在(权限不足时也会返回错误,视为不可确认)。
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	return p.Signal(syscall.Signal(0)) == nil
}
