package auth

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ErrAgentUnavailable 表示 ssh-agent 不可用(未启动、管道不存在、socket 未设置)。
// 调用方据此在 agent 与 key/password 之间优雅降级(README §10.2)。
var ErrAgentUnavailable = errors.New("ssh-agent 不可用")

// WindowsAgentPipe 是 Windows OpenSSH ssh-agent 的命名管道路径(README §5.7 / §10.2)。
const WindowsAgentPipe = `\\.\pipe\openssh-ssh-agent`

// AgentSocket 返回本机 ssh-agent 的接入地址:
// Windows 为命名管道,其余平台为 $SSH_AUTH_SOCK。空串表示未配置。
func AgentSocket() string {
	if runtime.GOOS == "windows" {
		return WindowsAgentPipe
	}
	return os.Getenv("SSH_AUTH_SOCK")
}

// agentMethods 连接本机 ssh-agent 并返回公钥回调认证方式。
// 连接失败时返回包装了 ErrAgentUnavailable 的可读错误。
func agentMethods() ([]ssh.AuthMethod, error) {
	conn, err := dialAgent()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
	}
	client := agent.NewClient(conn)
	// PublicKeysCallback 在握手时才向 agent 索取签名,agent 中的密钥增删即时生效。
	return []ssh.AuthMethod{ssh.PublicKeysCallback(client.Signers)}, nil
}

// IsAgentUnavailable 判断错误是否源于 ssh-agent 不可用,供调用方降级。
func IsAgentUnavailable(err error) bool {
	return errors.Is(err, ErrAgentUnavailable)
}
