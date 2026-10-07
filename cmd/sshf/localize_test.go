package main

import (
	"bytes"
	"strings"
	"testing"
)

// runRoot 用给定参数执行命令树,返回合并后的输出与错误。
func runRoot(args ...string) (string, error) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestRootHelpIsLocalized(t *testing.T) {
	out, err := runRoot("--help")
	if err != nil {
		t.Fatalf("--help 应成功: %v", err)
	}
	for _, want := range []string{"用法:", "可用命令:", "选项:", "显示帮助信息", "显示版本信息", "查看任意命令的帮助"} {
		if !strings.Contains(out, want) {
			t.Errorf("根帮助缺少中文片段 %q\n---\n%s", want, out)
		}
	}
	for _, bad := range []string{"Available Commands", "Flags:", "Usage:", "help for", "version for"} {
		if strings.Contains(out, bad) {
			t.Errorf("根帮助仍含英文 %q\n---\n%s", bad, out)
		}
	}
}

func TestSubcommandHelpIsLocalized(t *testing.T) {
	out, err := runRoot("exec", "--help")
	if err != nil {
		t.Fatalf("exec --help 应成功: %v", err)
	}
	for _, want := range []string{"用法:", "选项:", "全局选项:", "单机超时"} {
		if !strings.Contains(out, want) {
			t.Errorf("exec 帮助缺少中文片段 %q\n---\n%s", want, out)
		}
	}
}

func TestVersionOutput(t *testing.T) {
	out, err := runRoot("--version")
	if err != nil {
		t.Fatalf("--version 应成功: %v", err)
	}
	if strings.TrimSpace(out) != "sshf v"+version {
		t.Errorf("版本输出应为 %q, got %q", "sshf v"+version, out)
	}
}

func TestErrorsAreLocalized(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"未知命令", []string{"bogus"}, "未知命令"},
		{"未知选项", []string{"exec", "--nope"}, "未知选项"},
		{"参数过少", []string{"exec", "only-host"}, "需要 2 个参数"},
		{"参数过多", []string{"hosts", "list", "extra"}, "不接受位置参数"},
		{"未知短选项", []string{"exec", "-z", "h", "c"}, "未知短选项"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runRoot(c.args...)
			if err == nil {
				t.Fatalf("%v 应报错", c.args)
			}
			// main 在打印前调用 localizeError;测试据此断言最终展示文案。
			if got := localizeError(err).Error(); !strings.Contains(got, c.want) {
				t.Errorf("错误应含 %q, got %q", c.want, got)
			}
		})
	}
}

func TestCompletionCommandGeneratesScript(t *testing.T) {
	out, err := runRoot("completion", "bash")
	if err != nil {
		t.Fatalf("completion bash 应成功: %v", err)
	}
	if !strings.Contains(out, "sshf") {
		t.Errorf("补全脚本应提及 sshf, got 前 80 字: %q", out[:min(80, len(out))])
	}

	if _, err := runRoot("completion", "nope"); err == nil {
		t.Fatal("未知 shell 应报错")
	}
}
