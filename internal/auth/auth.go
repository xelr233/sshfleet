// Package auth 负责把主机配置里的凭据字段翻译成 SSH 认证方式,并加载私钥。
//
// 凭据优先级(README §5.7):ssh-agent > 私钥文件 > 密码。
// 本包只做「配置 → ssh.AuthMethod / ssh.Signer」的转换,不发起任何连接,
// 因此可以完全离线单测(用临时生成的密钥文件)。
package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/xelr233/sshfleet/internal/config"
)

// AuthMethods 按优先级组装主机的认证方式:agent > 私钥 > 密码。
// 至少需要一种凭据,否则返回错误(而不是让 SSH 握手时给出含糊的失败)。
//
// 注意:返回的切片顺序即 ssh 客户端的尝试顺序。
func AuthMethods(h *config.Host) ([]ssh.AuthMethod, error) {
	if h == nil {
		return nil, fmt.Errorf("auth: 主机配置为空")
	}
	var methods []ssh.AuthMethod

	if h.Agent {
		am, err := agentMethods()
		if err != nil {
			return nil, err
		}
		methods = append(methods, am...)
	}
	if h.Key != "" {
		signer, err := LoadSigner(h.Key, h.Passphrase)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if h.Password != "" {
		methods = append(methods, ssh.Password(h.Password))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("auth: 主机 %q 未配置任何凭据(agent / key / password)", h.Name)
	}
	return methods, nil
}

// ExpandPath 展开路径开头的 ~(仅当前用户),返回绝对或原样路径。
// hosts.yaml 里写 ~/.ssh/id_ed25519 是常见写法,必须支持。
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("auth: 路径为空")
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("auth: 无法确定用户主目录以展开 %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	rest := p[2:]
	return filepath.Join(home, filepath.FromSlash(rest)), nil
}

// keyCandidateNames 是枚举目录时按优先级尝试的私钥文件名。
// 顺序即优先级:更现代的算法在前。
var keyCandidateNames = []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"}

// EnumerateKeys 在 dir 下按优先级枚举存在的私钥文件,返回其绝对路径。
// 用于「用户只配置了 user/addr、私钥在 ~/.ssh 里」的场景(参考 GoSSHa 的 makeSigners)。
// 只返回文件名命中候选列表且能解析为私钥的路径。
func EnumerateKeys(dir string) []string {
	var out []string
	for _, name := range keyCandidateNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// 能解析出来的才算私钥,避免把同名的公钥/杂项文件纳入。
		if _, err := LoadSigner(p, ""); err == nil {
			out = append(out, p)
			continue
		}
		// 加密私钥会因缺少口令而失败,但仍是有效候选;此处仅校验 PEM/OpenSSH 头。
		if isPrivateKeyFile(p) {
			out = append(out, p)
		}
	}
	return out
}

// isPrivateKeyFile 粗略判断文件是否为私钥(含 PRIVATE KEY 标记)。
func isPrivateKeyFile(p string) bool {
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "PRIVATE KEY")
}
