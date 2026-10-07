package config

import (
	"os"
	"path/filepath"
)

// HomeEnv 覆盖状态目录位置,便于测试与多实例隔离。
const HomeEnv = "SSHFLEET_HOME"

// Dir 返回状态目录(~/.sshfleet)。不创建目录,只计算路径。
func Dir() string {
	if v := os.Getenv(HomeEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// 退回当前目录下的 .sshfleet,保证函数永不 panic。
		return ".sshfleet"
	}
	return filepath.Join(home, ".sshfleet")
}

// ConfigPath 是 hosts.yaml 路径。
func ConfigPath() string { return filepath.Join(Dir(), "hosts.yaml") }

// TokenPath 是 daemon 启动时生成的一次性 IPC token 路径(README §5.1)。
func TokenPath() string { return filepath.Join(Dir(), "token") }

// PIDPath 是 daemon 的 PID 文件路径。
func PIDPath() string { return filepath.Join(Dir(), "daemon.pid") }

// PortPath 是 daemon 监听端口文件路径。
func PortPath() string { return filepath.Join(Dir(), "daemon.port") }

// EnsureDir 创建状态目录(0700)。
func EnsureDir() error {
	return os.MkdirAll(Dir(), 0o700)
}
