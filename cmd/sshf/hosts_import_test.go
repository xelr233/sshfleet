package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelr233/sshfleet/internal/config"
)

const sampleSSHConfig = `
# 注释
Host web-1
    HostName 10.0.0.11
    User deploy
    Port 2222
    IdentityFile ~/.ssh/id_ed25519

Host db-1
    HostName 10.0.0.21

Host *
    Port 2200

Host bastion
    HostName 203.0.113.9
    ProxyJump none-real
`

func writeSSHConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(path, []byte(sampleSSHConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostsImportWritesConfig(t *testing.T) {
	sshCfg := writeSSHConfig(t)
	hostsPath := filepath.Join(t.TempDir(), "hosts.yaml")

	out, err := runRoot("--config", hostsPath, "hosts", "import", "--file", sshCfg)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	if !strings.Contains(out, "已写入") {
		t.Fatalf("应提示已写入, got:\n%s", out)
	}
	// Host * 是通配,应告警。
	if !strings.Contains(out, "警告") {
		t.Errorf("应输出通配告警, got:\n%s", out)
	}

	f, err := config.Load(hostsPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	web := f.Hosts["web-1"]
	if web == nil || web.Addr != "10.0.0.11" || web.User != "deploy" || web.Port != 2222 || web.Key != "~/.ssh/id_ed25519" {
		t.Fatalf("web-1 导入错误: %+v", web)
	}
	// db-1 未写 Port,但 Host * 在后,按首值生效仍应取到 2200。
	if db := f.Hosts["db-1"]; db == nil || db.Addr != "10.0.0.21" || db.Port != 2200 {
		t.Fatalf("db-1 导入错误: %+v", db)
	}
}

func TestHostsImportSkipsExistingWithoutOverwrite(t *testing.T) {
	sshCfg := writeSSHConfig(t)
	hostsPath := filepath.Join(t.TempDir(), "hosts.yaml")

	if _, err := runRoot("--config", hostsPath, "hosts", "import", "--file", sshCfg); err != nil {
		t.Fatalf("首次 import: %v", err)
	}
	out, err := runRoot("--config", hostsPath, "hosts", "import", "--file", sshCfg)
	if err != nil {
		t.Fatalf("二次 import: %v", err)
	}
	if !strings.Contains(out, "跳过") || !strings.Contains(out, "无变更") {
		t.Fatalf("二次导入应全部跳过, got:\n%s", out)
	}
}

func TestHostsImportOverwriteAndGroup(t *testing.T) {
	sshCfg := writeSSHConfig(t)
	hostsPath := filepath.Join(t.TempDir(), "hosts.yaml")

	if _, err := runRoot("--config", hostsPath, "hosts", "import", "--file", sshCfg); err != nil {
		t.Fatalf("首次 import: %v", err)
	}
	out, err := runRoot("--config", hostsPath, "hosts", "import", "--file", sshCfg, "--overwrite", "--group", "web")
	if err != nil {
		t.Fatalf("覆盖 import: %v\n%s", err, out)
	}
	if !strings.Contains(out, "更新") {
		t.Fatalf("应报告更新, got:\n%s", out)
	}

	f, err := config.Load(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	group := map[string]bool{}
	for _, m := range f.Groups["web"] {
		group[m] = true
	}
	if !group["web-1"] || !group["db-1"] {
		t.Fatalf("分组 web 应含 web-1/db-1, got %+v", f.Groups)
	}
}

func TestHostsImportDryRunDoesNotWrite(t *testing.T) {
	sshCfg := writeSSHConfig(t)
	hostsPath := filepath.Join(t.TempDir(), "hosts.yaml")

	out, err := runRoot("--config", hostsPath, "hosts", "import", "--file", sshCfg, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Fatalf("应提示 dry-run, got:\n%s", out)
	}
	if _, err := os.Stat(hostsPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应创建 hosts.yaml, stat err=%v", err)
	}
}
