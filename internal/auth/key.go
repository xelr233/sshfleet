package auth

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// LoadSigner 从 path 读取并解析 OpenSSH 格式私钥。
//
// passphrase 为空但私钥已加密时,返回带明确提示的错误(引导用户在 hosts.yaml
// 填写 passphrase),而不是让 ssh 握手阶段给出难以理解的失败。
func LoadSigner(path, passphrase string) (ssh.Signer, error) {
	p, err := ExpandPath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("auth: 读取私钥 %s: %w", p, err)
	}
	return ParseSigner(data, passphrase, p)
}

// ParseSigner 解析私钥字节。origin 仅用于错误信息(如文件路径)。
func ParseSigner(data []byte, passphrase, origin string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, nil
	}

	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, fmt.Errorf("auth: 解析私钥 %s: %w", origin, err)
	}
	// 私钥已加密。
	if passphrase == "" {
		return nil, fmt.Errorf("auth: 私钥 %s 已加密,请在主机配置中设置 passphrase", origin)
	}
	signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	if err != nil {
		return nil, fmt.Errorf("auth: 私钥 %s 口令错误或密钥损坏: %w", origin, err)
	}
	return signer, nil
}
