//go:build windows

package auth

import (
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialAgent 通过 Windows 命名管道连接 OpenSSH ssh-agent。
// 要求 ssh-agent 服务已启动(README §10.2);服务未启动时 DialPipe 会直接报错,
// 由 agentMethods 包装成 ErrAgentUnavailable。
func dialAgent() (net.Conn, error) {
	conn, err := winio.DialPipe(WindowsAgentPipe, ptr(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("连接 ssh-agent 管道 %s(ssh-agent 服务是否已启动?): %w", WindowsAgentPipe, err)
	}
	return conn, nil
}

func ptr[T any](v T) *T { return &v }
