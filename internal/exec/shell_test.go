package exec

import (
	"strings"
	"testing"

	"github.com/xelr233/sshfleet/internal/config"
)

func TestBuildInvocationAutoPassesThrough(t *testing.T) {
	// 关键默认:auto 必须原样传递命令,不包裹 —— 目标可能是 cmd.exe / PowerShell /
	// bash,硬编码 `/bin/bash -c` 会在 Windows 目标上直接失败。
	for _, kind := range []string{"", config.ShellAuto} {
		inv, err := BuildInvocation(kind, "df -h | head")
		if err != nil {
			t.Fatalf("BuildInvocation(%q): %v", kind, err)
		}
		if inv.Command != "df -h | head" {
			t.Fatalf("auto 应原样传递, got %q", inv.Command)
		}
		if inv.Stdin != "" {
			t.Fatalf("auto 不应注入 stdin, got %q", inv.Stdin)
		}
	}
}

func TestBuildInvocationBashViaStdin(t *testing.T) {
	inv, err := BuildInvocation(config.ShellBash, `echo "a b" && ls -l`)
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if inv.Command != "bash -s" {
		t.Fatalf("bash 应走 bash -s, got %q", inv.Command)
	}
	// 脚本经 stdin 传入:命令内容不经过外层 shell 的引号解析。
	if inv.Stdin != "echo \"a b\" && ls -l\n" {
		t.Fatalf("bash 脚本应经 stdin 且带尾换行, got %q", inv.Stdin)
	}
}

func TestBuildInvocationShAndPowerShell(t *testing.T) {
	cases := map[string]string{
		config.ShellSh:         "sh -s",
		config.ShellPowerShell: "powershell -NoProfile -NonInteractive -Command -",
		config.ShellPwsh:       "pwsh -NoProfile -NonInteractive -Command -",
	}
	for kind, wantCmd := range cases {
		inv, err := BuildInvocation(kind, "Get-ChildItem")
		if err != nil {
			t.Fatalf("BuildInvocation(%q): %v", kind, err)
		}
		if inv.Command != wantCmd {
			t.Fatalf("%s: got command %q want %q", kind, inv.Command, wantCmd)
		}
		if !strings.HasSuffix(inv.Stdin, "\n") {
			t.Fatalf("%s: 脚本应以换行结尾, got %q", kind, inv.Stdin)
		}
	}
}

func TestBuildInvocationCmd(t *testing.T) {
	inv, err := BuildInvocation(config.ShellCmd, "dir /b")
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if inv.Command != "cmd /c dir /b" {
		t.Fatalf("cmd 应走 cmd /c, got %q", inv.Command)
	}
	if inv.Stdin != "" {
		t.Fatalf("cmd 不走 stdin, got %q", inv.Stdin)
	}
}

func TestBuildInvocationTrailingNewlineNotDoubled(t *testing.T) {
	inv, err := BuildInvocation(config.ShellBash, "echo hi\n")
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if inv.Stdin != "echo hi\n" {
		t.Fatalf("已有结尾换行不应再加, got %q", inv.Stdin)
	}
}

func TestBuildInvocationUnknownShell(t *testing.T) {
	if _, err := BuildInvocation("fish", "ls"); err == nil {
		t.Fatal("未知 shell 应报错")
	}
}

func TestShellKindsAllBuildable(t *testing.T) {
	// config 校验允许的每种 shell 都必须能构造出 invocation,避免两处清单漂移。
	for _, kind := range []string{
		config.ShellAuto, config.ShellBash, config.ShellSh,
		config.ShellCmd, config.ShellPowerShell, config.ShellPwsh,
	} {
		if _, err := BuildInvocation(kind, "true"); err != nil {
			t.Fatalf("config 允许的 shell %q 无法构造 invocation: %v", kind, err)
		}
	}
}
