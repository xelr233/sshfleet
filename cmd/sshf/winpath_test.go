package main

import (
	"runtime"
	"strings"
	"testing"
)

// skipUnlessGitBash 跳过只在 Windows + Git Bash 下才有意义的用例:
// 参数转换是 MSYS2 在 Windows 上的行为,Linux CI 上不存在。
func skipUnlessGitBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("MSYS2 参数转换只在 Windows 上发生")
	}
	t.Setenv("MSYSTEM", "MINGW64")
}

func TestLooksLikeWindowsPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"C:/Users/me/a.zip", true},
		{"c:\\Users\\me\\a.zip", true},
		{"C:", true},
		{"\\\\srv\\share\\a.zip", true},
		{"/tmp/a.zip", false},
		{"//tmp/a.zip", false}, // MSYS 里给 POSIX 路径加前导 // 正是防转换写法
		{"C:relative", false},
		{"./a.zip", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeWindowsPath(c.in); got != c.want {
			t.Errorf("looksLikeWindowsPath(%q) = %v,期望 %v", c.in, got, c.want)
		}
	}
}

func TestSamePathText(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"C:/Users/me/Temp/a.zip", "c:\\users\\me\\temp\\a.zip", true},
		{"C:/Users/me/Temp/a.zip/", "C:/Users/me/Temp/a.zip", true},
		{"C:/Users/me/Temp/a.zip", "C:/Users/me/Temp/b.zip", false},
		{"/tmp/a.zip", "/tmp/a.zip", true},
	}
	for _, c := range cases {
		if got := samePathText(c.a, c.b); got != c.want {
			t.Errorf("samePathText(%q, %q) = %v,期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// 复现用户报告的场景:Git Bash 把 /tmp/a.zip 改写成 %TEMP% 下的 Windows 路径,
// CLI 收到的远端已经是 Windows 形式,继续传输就会写到服务器的 /root/C:/... 。
func TestGuardRemotePathRejectsWindowsRemoteUnderMSYS(t *testing.T) {
	skipUnlessGitBash(t)
	mangled := "C:/Users/admin/AppData/Local/Temp/a.zip"
	err := guardRemotePath("upload", "E:/build/a.zip", mangled, false)
	if err == nil {
		t.Fatal("Git Bash 下 Windows 形式远端应报错,否则会静默写错位置")
	}
	for _, want := range []string{"远端路径", "--allow-windows-remote", "MSYS2_ARG_CONV_EXCL", "MSYS_NO_PATHCONV"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q:\n%s", want, err)
		}
	}
}

// 目标确实是 Windows 主机时,显式加 --allow-windows-remote 即按原样发送。
func TestGuardRemotePathAllowsExplicitWindowsRemote(t *testing.T) {
	skipUnlessGitBash(t)
	if err := guardRemotePath("upload", "E:/build/a.zip", "C:/Users/me/a.zip", true); err != nil {
		t.Fatalf("--allow-windows-remote 应放行: %v", err)
	}
}

// 本地与远端完全相同(不论是否 MSYS)是笔误或转换产物,必须拦下。
func TestGuardRemotePathRejectsIdenticalPaths(t *testing.T) {
	t.Setenv("MSYSTEM", "")
	t.Setenv("MSYS", "")
	same := "C:/Users/admin/AppData/Local/Temp/a.zip"
	if err := guardRemotePath("upload", same, same, false); err == nil {
		t.Fatal("本地与远端相同时应报错")
	}
}

// 没有 MSYS 转换机制时(cmd/PowerShell/Linux),Windows 形式远端可能真的指向
// Windows 主机,不应打扰。
func TestGuardRemotePathSilentWithoutConversion(t *testing.T) {
	t.Setenv("MSYSTEM", "")
	t.Setenv("MSYS", "")
	if err := guardRemotePath("upload", "E:/build/app.exe", "C:/Users/me/app.exe", false); err != nil {
		t.Fatalf("非 MSYS 环境不应报错: %v", err)
	}
}

// POSIX 远端路径原样放行。
func TestGuardRemotePathIgnoresPOSIXRemote(t *testing.T) {
	if err := guardRemotePath("upload", "E:/build/app.zip", "/tmp/app.zip", false); err != nil {
		t.Fatalf("POSIX 远端不应报错: %v", err)
	}
}

// 命令层接线:传输类命令必须在联系 daemon 之前完成拦截。
func TestTransferCommandsRejectMangledRemote(t *testing.T) {
	skipUnlessGitBash(t)
	mangled := "C:/Users/admin/AppData/Local/Temp/a.zip"
	cases := [][]string{
		{"upload", "rocky-tx", mangled, mangled},
		{"download", "rocky-tx", mangled, mangled},
		{"distribute", mangled, mangled, "--host", "rocky-tx"},
	}
	for _, args := range cases {
		out, err := runRoot(args...)
		if err == nil {
			t.Errorf("%v 应被拦截,实际输出 %q", args, out)
			continue
		}
		if !strings.Contains(err.Error(), "--allow-windows-remote") {
			t.Errorf("%v 的报错应给出绕过办法,实际: %v", args, err)
		}
	}
}

// 三个传输命令都必须提供 --allow-windows-remote 开关。
func TestTransferCommandsExposeAllowWindowsRemoteFlag(t *testing.T) {
	for _, name := range []string{"upload", "download", "distribute"} {
		out, err := runRoot(name, "--help")
		if err != nil {
			t.Fatalf("%s --help 应成功: %v", name, err)
		}
		if !strings.Contains(out, "--allow-windows-remote") {
			t.Errorf("%s --help 缺少 --allow-windows-remote:\n%s", name, out)
		}
	}
}
