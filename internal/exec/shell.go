// Package exec 实现单机 exec 与多机 batch。
//
// 与「硬编码 /bin/bash -c」的常见做法不同,本包默认把命令原样交给目标机的
// 登录 shell(与 `ssh host cmd` 一致):目标可能是 Windows cmd.exe、PowerShell,
// 也可能是 bash/sh,不能假设。需要确定性 shell 时用 host 配置的 shell 字段或
// exec 的 --shell 标志显式指定,见 BuildInvocation。
package exec

import (
	"fmt"
	"strings"

	"github.com/xelr233/sshfleet/internal/config"
)

// Invocation 描述一条命令在远端的执行形式。
type Invocation struct {
	// Command 是通过 SSH exec 通道发送的命令字符串。
	Command string
	// Stdin 非空时需写入会话 stdin(用于把脚本喂给解释器,规避引号与转义差异)。
	Stdin string
}

// BuildInvocation 依据 shell 种类把 cmd 组装成远端执行形式。
//
// kind 为空或 "auto" 时原样返回 —— 命令字符串直接交给目标机登录 shell,
// 这是跨平台安全默认。其余取值会显式启动解释器:
//
//	bash/sh          → `bash -s` / `sh -s`,脚本经 stdin 传入
//	powershell/pwsh  → `<interp> -NoProfile -NonInteractive -Command -`,脚本经 stdin 传入
//	cmd              → `cmd /c <cmd>`,交给 cmd 自行解析
//
// bash/sh/powershell 走 stdin 而非 `-c`,是因为这样命令内容完全不经过外层
// shell 的引号解析,含空格、引号、$、% 的复杂命令不会破坏(README §10.5 的反面)。
func BuildInvocation(kind, cmd string) (Invocation, error) {
	switch kind {
	case "", config.ShellAuto:
		// 关键默认:不包裹。交命令给登录 shell,行为等同 `ssh host <cmd>`。
		return Invocation{Command: cmd}, nil

	case config.ShellBash:
		return Invocation{Command: "bash -s", Stdin: ensureTrailingNewline(cmd)}, nil
	case config.ShellSh:
		return Invocation{Command: "sh -s", Stdin: ensureTrailingNewline(cmd)}, nil

	case config.ShellPowerShell:
		return Invocation{Command: "powershell -NoProfile -NonInteractive -Command -", Stdin: ensureTrailingNewline(cmd)}, nil
	case config.ShellPwsh:
		return Invocation{Command: "pwsh -NoProfile -NonInteractive -Command -", Stdin: ensureTrailingNewline(cmd)}, nil

	case config.ShellCmd:
		return Invocation{Command: "cmd /c " + cmd}, nil

	default:
		return Invocation{}, fmt.Errorf("exec: 未知 shell %q(可选: auto/bash/sh/cmd/powershell/pwsh)", kind)
	}
}

// ensureTrailingNewline 保证脚本以换行结尾,否则解释器可能丢弃最后一条命令。
func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
