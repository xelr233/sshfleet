package auth

import (
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/xelr233/sshfleet/internal/config"
)

// defaultCiphers 是协商优先顺序(README §5.5):先 GCM,再 chacha20,最后 CTR 兜底。
// 不包含 CBC 系列(某些旧实现有 padding oracle 隐患)。
var defaultCiphers = []string{
	"aes128-gcm@openssh.com",
	"aes256-gcm@openssh.com",
	"chacha20-poly1305@openssh.com",
	"aes128-ctr",
	"aes192-ctr",
	"aes256-ctr",
}

// ConfigOptions 是组装 ssh.ClientConfig 的可调项。
type ConfigOptions struct {
	// Timeout 限制 TCP 连接 + 握手的总时长,0 表示不设限。
	Timeout time.Duration
	// HostKeyCallback 校验目标主机密钥。为零值时使用 InsecureIgnoreHostKey,
	// 并在调用方由 pool 记录一次告警(known_hosts 校验计划在 M6 落地)。
	HostKeyCallback ssh.HostKeyCallback
	// Ciphers 覆盖默认加密套件顺序;为空用 defaultCiphers。
	Ciphers []string
}

// BuildClientConfig 把主机配置组装成 ssh.ClientConfig。
//
// 第二个返回值是凭据降级告警(如有),调用方应展示给用户,而不是静默吞掉。
//
// 注意:ClientConfig 不开启 zlib 压缩 —— x/crypto/ssh 默认即为 "none",
// 符合 README §5.5「大文件分发时压缩纯浪费 CPU」的决策;如需压缩应在
// 目标机侧用更合适的协议,而不是 SSH 通道压缩。
func BuildClientConfig(h *config.Host, o ConfigOptions) (*ssh.ClientConfig, []string, error) {
	if h == nil {
		return nil, nil, fmt.Errorf("auth: 主机配置为空")
	}
	methods, warnings, err := AuthMethods(h)
	if err != nil {
		return nil, warnings, err
	}

	hostKey := o.HostKeyCallback
	if hostKey == nil {
		// 已知折衷:未做 known_hosts 校验(README §10 风险清单类)。先用不安全
		// 回调解锁功能,校验计划随 M6 落地;pool 会记录告警。
		hostKey = ssh.InsecureIgnoreHostKey() //nolint:gosec // 见上
	}

	ciphers := o.Ciphers
	if len(ciphers) == 0 {
		ciphers = h.Cipher
	}
	if len(ciphers) == 0 {
		ciphers = defaultCiphers
	}

	return &ssh.ClientConfig{
		User:            h.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         o.Timeout,
		Ciphers:         ciphers,
	}, warnings, nil
}
