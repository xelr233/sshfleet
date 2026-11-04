package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// 本文件处理 Git Bash / MSYS2 的「参数自动转换」:
//
// 在这类 POSIX 层里启动原生 Windows 程序时,形如 /tmp/a.zip 的参数会被运行时
// 改写成本地 Windows 路径,程序拿到的已经不是用户敲的字符串:
//
//	sshf upload rocky-tx ./a.zip /tmp/a.zip
//	# CLI 实际发出:{"local":"...","remote":"C:/Users/me/AppData/Local/Temp/a.zip"}
//
// 远端变成 Windows 路径后,SFTP 会在服务器上按相对路径写出 /root/C:/Users/...,
// 而用户以为的 /tmp/a.zip 根本没被写 —— 文件"上传成功"却不在预期位置。
// CLI 无法还原被改写的字符串,只能拦截 Windows 形式的远端路径,给出可操作的提示
// (README §10)。

// looksLikeWindowsPath 判断 s 是否是 Windows 绝对路径:盘符式(C:/x、C:\x)
// 或 UNC(\\server\share)。C:foo 这类盘符相对路径不算,它也可能是合法远端路径。
func looksLikeWindowsPath(s string) bool {
	if len(s) >= 2 && s[1] == ':' && isASCIILetter(s[0]) {
		return len(s) == 2 || s[2] == '/' || s[2] == '\\'
	}
	return len(s) > 2 && strings.HasPrefix(s, `\\`)
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// samePathText 按 Windows 语义比较两段路径文本:分隔符、大小写、尾随分隔符都不敏感。
func samePathText(a, b string) bool {
	return foldPathText(a) == foldPathText(b)
}

// foldPathText 把路径文本归一化到可比较形式(仅用于比较,不用于实际访问文件)。
func foldPathText(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	if len(s) > 1 {
		s = strings.TrimRight(s, "/")
	}
	return strings.ToLower(s)
}

// msysShell 判断当前进程是否由 Git Bash / MSYS2 之类的 POSIX 层启动 ——
// 只有这种环境才会在创建进程前改写参数。
func msysShell() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return os.Getenv("MSYSTEM") != "" || os.Getenv("MSYS") != ""
}

// guardRemotePath 拦截 Windows 形式的远端路径,避免把文件静默写到服务器上
// 意想不到的相对路径里。
//
// 判据:
//   - 远端不是 Windows 形式(如 /tmp/a.zip)→ 放行;
//   - 调用方显式声明目标是 Windows 主机(--allow-windows-remote)→ 放行;
//   - Git Bash / MSYS2 下出现 Windows 形式远端 → 极可能是 /tmp/x 被改写,报错;
//   - 非 MSYS 环境(cmd/PowerShell/Linux)但本地与远端路径完全相同 → 报错
//     (多半是参数写反),因为此时不存在参数转换,写反只可能是笔误。
//
// verb 只用于提示文本(如 "upload")。
func guardRemotePath(verb, local, remote string, allowWindowsRemote bool) error {
	if allowWindowsRemote || !looksLikeWindowsPath(remote) {
		return nil
	}
	if !msysShell() && !samePathText(local, remote) {
		// 没有参数转换机制时,Windows 形式远端要么确实指向 Windows 主机
		// (如从 cmd 上传),要么两边都写成了 Windows 路径 —— 不打扰。
		return nil
	}
	return fmt.Errorf("%s: 远端路径 %q 是 Windows 形式\n\n"+
		"Git Bash(MSYS2)会在启动 sshf 前把形如 /tmp/a.zip 的参数自动改写成本地\n"+
		"Windows 路径,sshf 收到的可能已经不是你写的远端路径。照原样传输会把文件\n"+
		"写进服务器当前目录下的相对路径(例如 /root/C:/Users/...),而你指定的\n"+
		"/tmp/a.zip 会被静默忽略。\n\n"+
		"若远端确实是 Windows 主机:  加 --allow-windows-remote 按原样使用该路径\n"+
		"若你写的是 POSIX 路径:      关闭 Git Bash 的参数转换,任选其一\n"+
		"  MSYS2_ARG_CONV_EXCL='*' sshf %s ...   # 关闭本次调用的参数转换\n"+
		"  MSYS_NO_PATHCONV=1 sshf %s ...        # Git for Windows 的旧开关\n"+
		"  改用 PowerShell / cmd 执行,或用原生 scp: scp <local> <host>:/tmp/a.zip",
		verb, remote, verb, verb)
}
