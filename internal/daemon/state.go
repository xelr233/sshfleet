package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/xelr233/sshfleet/internal/config"
)

// newToken 生成 32 字节随机 token 的十六进制串(README §5.1)。
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("daemon: 生成 token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// writeFile0600 写文件并尽量收紧权限(Windows 下权限位无效,靠 ACL;README §10.6)。
func writeFile0600(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("daemon: 写 %s: %w", path, err)
	}
	return nil
}

// readTrimmed 读取文件并去掉首尾空白,失败返回错误。
func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writeRuntimeState 落盘 daemon 的 PID 与端口,供 CLI 发现与 stop 使用。
func writeRuntimeState(pid, port int) error {
	if err := config.EnsureDir(); err != nil {
		return fmt.Errorf("daemon: 创建状态目录: %w", err)
	}
	if err := writeFile0600(config.PIDPath(), strconv.Itoa(pid)); err != nil {
		return err
	}
	return writeFile0600(config.PortPath(), strconv.Itoa(port))
}

// ClearRuntimeState 删除 PID / 端口 / token 文件,忽略不存在的情况。
func ClearRuntimeState() {
	for _, p := range []string{config.PIDPath(), config.PortPath(), config.TokenPath()} {
		_ = os.Remove(p)
	}
}

// ReadPort 读取已运行 daemon 的端口;文件不存在或非法时返回错误。
func ReadPort() (int, error) {
	s, err := readTrimmed(config.PortPath())
	if err != nil {
		return 0, fmt.Errorf("daemon: 读取端口文件: %w", err)
	}
	port, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("daemon: 端口文件内容非法 %q: %w", s, err)
	}
	return port, nil
}

// ReadPID 读取已运行 daemon 的 PID。
func ReadPID() (int, error) {
	s, err := readTrimmed(config.PIDPath())
	if err != nil {
		return 0, fmt.Errorf("daemon: 读取 PID 文件: %w", err)
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("daemon: PID 文件内容非法 %q: %w", s, err)
	}
	return pid, nil
}

// ReadToken 读取 daemon 生成的 IPC token。
func ReadToken() (string, error) {
	s, err := readTrimmed(config.TokenPath())
	if err != nil {
		return "", fmt.Errorf("daemon: 读取 token: %w", err)
	}
	if s == "" {
		return "", fmt.Errorf("daemon: token 文件为空")
	}
	return s, nil
}
