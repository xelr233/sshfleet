//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// processAlive 通过 tasklist 判断进程是否存在。Windows 上 os.FindProcess 恒成功,
// 因此不能用它探测存活。
func processAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return false
	}
	// CSV 首列为镜像名(带引号),比对 PID 列更稳妥;这里在所有字段里找 PID。
	want := strconv.Itoa(pid)
	for _, f := range strings.FieldsFunc(string(out), func(r rune) bool {
		return r == ',' || r == '"' || r == ' ' || r == '\r' || r == '\n'
	}) {
		if f == want {
			return true
		}
	}
	return false
}
