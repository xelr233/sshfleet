package auth

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/crypto/ssh"
)

// ErrAgentNotImplemented 标记 ssh-agent 接入尚未落地(计划于 M1)。
var ErrAgentNotImplemented = errors.New("ssh-agent 接入尚未实现")

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

// agentMethods 组装 ssh-agent 认证方式。
//
// TODO(M1): 完成实现后此处应:
//  1. Unix:net.Dial("unix", $SSH_AUTH_SOCK) + agent.NewClient 得到 agent.Agent,
//     再包成 ssh.PublicKeysCallback(agent.Signers) 返回。
//  2. Windows:用 github.com/Microsoft/go-winio 的 winio.DialPipe(WindowsAgentPipe)
//     得到 net.Conn,其余同上(README §2 已列该依赖)。
//  3. ssh-agent 服务未启动 / 管道不存在时,按 README §10.2 优雅降级:
//     返回可读错误(当前即是这种形态),由调用方决定是否回退到 key/password。
func agentMethods() ([]ssh.AuthMethod, error) {
	sock := AgentSocket()
	if sock == "" {
		return nil, fmt.Errorf("auth: 主机启用了 agent,但未找到 ssh-agent 地址(Unix 需设置 SSH_AUTH_SOCK,Windows 需启动 ssh-agent 服务):%w", ErrAgentNotImplemented)
	}
	return nil, fmt.Errorf("auth: 检测到 ssh-agent 地址 %s,但接入尚未实现:%w", sock, ErrAgentNotImplemented)
}
