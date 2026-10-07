package sshconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func entry(t *testing.T, cfg *Config, alias string) Entry {
	t.Helper()
	for _, e := range cfg.Entries {
		if e.Alias == alias {
			return e
		}
	}
	t.Fatalf("未找到别名 %q(got %+v)", alias, cfg.Entries)
	return Entry{}
}

func TestParseBasic(t *testing.T) {
	cfg := parse(t, `
# 注释行
Host web-1
    HostName 10.0.0.11
    User deploy
    Port 2222
    IdentityFile ~/.ssh/id_ed25519

Host db-1
    HostName 10.0.0.21
`)
	if len(cfg.Entries) != 2 {
		t.Fatalf("应有 2 个别名, got %+v", cfg.Entries)
	}
	web := entry(t, cfg, "web-1")
	if web.HostName != "10.0.0.11" || web.User != "deploy" || web.Port != 2222 || web.IdentityFile != "~/.ssh/id_ed25519" {
		t.Fatalf("web-1 解析错误: %+v", web)
	}
	db := entry(t, cfg, "db-1")
	if db.HostName != "10.0.0.21" || db.Port != 0 || db.User != "" {
		t.Fatalf("db-1 解析错误: %+v", db)
	}
}

func TestHostNameDefaultsToAlias(t *testing.T) {
	cfg := parse(t, "Host lone\n    User ops\n")
	e := entry(t, cfg, "lone")
	if e.HostName != "" {
		t.Fatalf("未写 HostName 时应留空(由导入层用 Alias 兜底), got %q", e.HostName)
	}
	if e.User != "ops" {
		t.Fatalf("User 应被解析, got %q", e.User)
	}
}

func TestGlobalBlockAppliesAndWins(t *testing.T) {
	// 文件开头的全局 User 先出现,按 OpenSSH「首个取值生效」,应压过 Host 块里的 User。
	cfg := parse(t, `
User globaluser
Host web
    HostName 10.0.0.11
    User blockuser

Host db
    HostName 10.0.0.21
`)
	if e := entry(t, cfg, "web"); e.User != "globaluser" {
		t.Fatalf("全局 User 应先生效, got %q", e.User)
	}
	if e := entry(t, cfg, "db"); e.User != "globaluser" {
		t.Fatalf("全局 User 应作用于 db, got %q", e.User)
	}
	if e := entry(t, cfg, "web"); e.HostName != "10.0.0.11" {
		t.Fatalf("HostName 应保留, got %q", e.HostName)
	}
}

func TestWildcardAndNegation(t *testing.T) {
	cfg := parse(t, `
Host web-1 db-1
    User ops

Host *
    Port 2200
    User fallback

Host !web-1
    Port 9999
`)
	// 通配 Host * 应作用于所有;但 User 已被前面的 web-1/db-1 块占用(首值生效)。
	if e := entry(t, cfg, "web-1"); e.Port != 2200 {
		t.Fatalf("Host * 的 Port 应作用于 web-1, got %d", e.Port)
	}
	if e := entry(t, cfg, "db-1"); e.User != "ops" || e.Port != 2200 {
		t.Fatalf("db-1 解析错误: %+v", e)
	}
	// !web-1 块应被否定,web-1 不取它的 Port(仍为 Host * 的 2200)。
	if e := entry(t, cfg, "web-1"); e.Port != 2200 {
		t.Fatalf("否定块不应作用于 web-1, got Port %d", e.Port)
	}
}

func TestFirstValueWinsAcrossBlocks(t *testing.T) {
	cfg := parse(t, `
Host web
    HostName 1.1.1.1
Host web
    HostName 2.2.2.2
    Port 2222
`)
	e := entry(t, cfg, "web")
	if e.HostName != "1.1.1.1" {
		t.Fatalf("同一别名的首个 HostName 应生效, got %q", e.HostName)
	}
	if e.Port != 2222 {
		t.Fatalf("后块补充的 Port 应生效, got %d", e.Port)
	}
}

func TestKeyValueSyntaxAndQuotes(t *testing.T) {
	cfg := parse(t, "Host h\n  HostName=1.2.3.4\n  Port=2200\n  IdentityFile=\"~/.ssh/my key\"\n")
	e := entry(t, cfg, "h")
	if e.HostName != "1.2.3.4" || e.Port != 2200 {
		t.Fatalf("Key=Value 语法解析错误: %+v", e)
	}
	if e.IdentityFile != "~/.ssh/my key" {
		t.Fatalf("引号应被去除, got %q", e.IdentityFile)
	}
}

func TestIdentityFileNoneIgnored(t *testing.T) {
	cfg := parse(t, "Host h\n  HostName 1.2.3.4\n  IdentityFile none\n")
	if e := entry(t, cfg, "h"); e.IdentityFile != "" {
		t.Fatalf("IdentityFile none 应被忽略, got %q", e.IdentityFile)
	}
}

func TestUnsupportedDirectivesRecorded(t *testing.T) {
	cfg := parse(t, `
Host jumpbox
    HostName 1.2.3.4
    ProxyJump bastion
    ProxyCommand ssh -W %h:%p bastion
`)
	e := entry(t, cfg, "jumpbox")
	if len(e.Unsupported) != 2 {
		t.Fatalf("应记录 proxyjump/proxycommand, got %v", e.Unsupported)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "jumpbox") {
		t.Fatalf("应有针对 jumpbox 的告警, got %v", cfg.Warnings)
	}
}

func TestIncludeAndMatchWarned(t *testing.T) {
	cfg := parse(t, `
Include ~/.ssh/conf.d/*
Host a
    HostName 1.1.1.1
Match host b
    User ops
`)
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "Include") {
		t.Fatalf("应告警 Include, got %v", cfg.Warnings)
	}
	if !strings.Contains(joined, "Match") {
		t.Fatalf("应告警 Match, got %v", cfg.Warnings)
	}
	if len(cfg.Entries) != 1 || cfg.Entries[0].Alias != "a" {
		t.Fatalf("Match 段不应产生别名, got %+v", cfg.Entries)
	}
}

func TestEmptyHostBlockIgnored(t *testing.T) {
	cfg := parse(t, "Host\n    User ops\nHost real\n  HostName 1.2.3.4\n")
	if len(cfg.Entries) != 1 || cfg.Entries[0].Alias != "real" {
		t.Fatalf("无模式的 Host 块不应被当成全局块, got %+v", cfg.Entries)
	}
}

func TestUnparsableLineErrors(t *testing.T) {
	if _, err := Parse([]byte("   =value\n")); err == nil {
		t.Fatal("非法行应报错")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("Host h\n  HostName 1.2.3.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Entries) != 1 {
		t.Fatalf("应有 1 个条目, got %+v", cfg.Entries)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("缺失文件应报错")
	}
}
