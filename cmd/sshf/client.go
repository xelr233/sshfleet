package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/xelr233/sshfleet/internal/config"
)

// configPath 由 root 的持久化 --config 标志解析;为空则用默认路径。
var configPath string

// loadConfig 读取 hosts.yaml。不存在时返回空配置(首次运行直接 add 即可)。
func loadConfig() (*config.File, error) {
	path, err := resolveConfigPath()
	if err != nil {
		return nil, err
	}
	return config.LoadOrEmpty(path)
}

// saveConfig 写回 hosts.yaml(原子写,0600)。
func saveConfig(f *config.File) error {
	path, err := resolveConfigPath()
	if err != nil {
		return err
	}
	return f.Save(path)
}

// resolveConfigPath 计算生效的 hosts.yaml 路径:显式 --config 优先,否则默认路径。
func resolveConfigPath() (string, error) {
	if configPath != "" {
		return configPath, nil
	}
	return config.ConfigPath(), nil
}

// isTerminal 判断 stdin 是否为交互终端,用于决定能否安全地发起提示。
// 避免在管道 / CI 场景下等待输入而挂起。
func isTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// prompt 在交互终端上打印提示并读入一行;非终端或读不到内容时返回错误。
func prompt(label string) (string, error) {
	if !isTerminal() {
		return "", fmt.Errorf("需要交互输入 %s,但当前非终端;请用相应命令行标志指定", label)
	}
	fmt.Fprintf(os.Stderr, "%s: ", label)
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return "", fmt.Errorf("读取 %s: %w", label, err)
	}
	return strings.TrimSpace(line), nil
}

// credentialKind 概括一台主机使用的凭据类型,用于 hosts list 展示。
func credentialKind(h *config.Host) string {
	var kinds []string
	if h.Agent {
		kinds = append(kinds, "agent")
	}
	if h.Key != "" {
		kinds = append(kinds, "key")
	}
	if h.Password != "" {
		kinds = append(kinds, "password")
	}
	if len(kinds) == 0 {
		return "-"
	}
	return strings.Join(kinds, "+")
}
