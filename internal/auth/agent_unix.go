//go:build !windows

package auth

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// dialAgent 连接 Unix 域套接字形式的 ssh-agent(地址取自 SSH_AUTH_SOCK)。
func dialAgent() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("未设置 SSH_AUTH_SOCK")
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 ssh-agent socket %s: %w", sock, err)
	}
	return conn, nil
}
