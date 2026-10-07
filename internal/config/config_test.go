package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sample = `
defaults:
  user: deploy
  port: 22
  agent: true

hosts:
  web-1:
    addr: 10.0.0.11
  web-2:
    addr: 10.0.0.12
    user: root
    key: ~/.ssh/id_ed25519
  db-1:
    addr: 10.0.0.21
    port: 2222
    password: hunter2

groups:
  web: [web-1, web-2]
  all: [web-1, web-2, db-1]
`

func mustParse(t *testing.T, s string) *File {
	t.Helper()
	f, err := Parse([]byte(s), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParseMergesDefaults(t *testing.T) {
	f := mustParse(t, sample)

	w1 := f.Hosts["web-1"]
	if w1.User != "deploy" { // 来自 defaults
		t.Fatalf("web-1 user 应继承 defaults, got %q", w1.User)
	}
	if w1.Port != 22 {
		t.Fatalf("web-1 port 应为 22, got %d", w1.Port)
	}
	if !w1.Agent { // defaults 的 agent: true 应继承
		t.Fatal("web-1 应继承 agent: true")
	}

	w2 := f.Hosts["web-2"]
	if w2.User != "root" { // 主机显式值优先于 defaults
		t.Fatalf("web-2 user 应覆盖 defaults, got %q", w2.User)
	}

	db := f.Hosts["db-1"]
	if db.Port != 2222 {
		t.Fatalf("db-1 port 应为 2222, got %d", db.Port)
	}
	if db.User != "deploy" {
		t.Fatalf("db-1 user 应继承 defaults, got %q", db.User)
	}
}

func TestNameFilledFromKey(t *testing.T) {
	f := mustParse(t, sample)
	if f.Hosts["web-1"].Name != "web-1" {
		t.Fatalf("Name 应由 map key 填充, got %q", f.Hosts["web-1"].Name)
	}
}

func TestResolveUnionAndSort(t *testing.T) {
	f := mustParse(t, sample)
	// 显式 web-2 + 分组 web(含 web-1, web-2)取并集并去重。
	got, err := f.Resolve([]string{"web-2"}, "web")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应去重为 2 台, got %d: %+v", len(got), got)
	}
	if got[0].Name != "web-1" || got[1].Name != "web-2" {
		t.Fatalf("结果应按名排序, got %q, %q", got[0].Name, got[1].Name)
	}
}

func TestResolveErrors(t *testing.T) {
	f := mustParse(t, sample)
	if _, err := f.Resolve(nil, ""); err == nil {
		t.Fatal("无选择条件应报错(防误操作全部主机)")
	}
	if _, err := f.Resolve([]string{"nope"}, ""); err == nil {
		t.Fatal("未知主机应报错")
	}
	if _, err := f.Resolve(nil, "nope"); err == nil {
		t.Fatal("未知分组应报错")
	}
}

func TestValidateRejectsBadRefsAndFields(t *testing.T) {
	cases := map[string]string{
		"缺 addr":     "hosts:\n  a:\n    user: x\n",
		"端口越界":       "hosts:\n  a:\n    addr: h\n    port: 70000\n",
		"分组引用不存在的主机": "hosts:\n  a:\n    addr: h\n    user: u\ngroups:\n  g: [a, ghost]\n",
	}
	for name, yaml := range cases {
		if _, err := Parse([]byte(yaml), "t"); err == nil {
			t.Fatalf("%s: 应报错", name)
		}
	}
}

func TestAddRemoveRoundTrip(t *testing.T) {
	f := mustParse(t, sample)

	if err := f.Add(&Host{Name: "new-1", Addr: "10.0.0.31", User: "ops"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if f.Hosts["new-1"] == nil {
		t.Fatal("Add 后主机缺失")
	}

	// 删除应同步清理分组引用。
	if err := f.Remove("web-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := f.Hosts["web-1"]; ok {
		t.Fatal("Remove 后主机仍在")
	}
	for g, members := range f.Groups {
		for _, m := range members {
			if m == "web-1" {
				t.Fatalf("分组 %q 仍引用已删除的 web-1", g)
			}
		}
	}
	if _, err := f.Resolve(nil, "web"); err != nil {
		t.Fatalf("web 分组删掉 web-1 后应仍有效: %v", err)
	}
}

func TestAddRollsBackOnInvalid(t *testing.T) {
	f := mustParse(t, sample)
	if err := f.Add(&Host{Name: "bad", Addr: ""}); err == nil {
		t.Fatal("缺 addr 的 Add 应报错")
	}
	if _, ok := f.Hosts["bad"]; ok {
		t.Fatal("校验失败的主机不应留在内存中")
	}
}

func TestPlaintextPasswordHosts(t *testing.T) {
	f := mustParse(t, sample)
	got := f.PlaintextPasswordHosts()
	if len(got) != 1 || got[0] != "db-1" {
		t.Fatalf("应只报出 db-1, got %v", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hosts.yaml") // 目录不存在,Save 应自行创建

	f := mustParse(t, sample)
	if err := f.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Windows 不实现 POSIX 权限位(可写文件一律显示 0666),需靠 ACL 收紧,
	// 见 README §10.6;此处只在 Unix 上断言 0600。
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("文件权限过宽: %o", perm)
		}
	}

	re, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(re.Hosts) != 3 {
		t.Fatalf("往返后主机数应为 3, got %d", len(re.Hosts))
	}
	if re.Hosts["db-1"].Port != 2222 {
		t.Fatalf("往返丢失端口: %+v", re.Hosts["db-1"])
	}
	if len(re.Groups["all"]) != 3 {
		t.Fatalf("往返丢失分组: %+v", re.Groups)
	}
}

func TestPathsRespectEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	if Dir() != dir {
		t.Fatalf("Dir 应尊重 %s, got %q", HomeEnv, Dir())
	}
	if !strings.HasPrefix(ConfigPath(), dir) {
		t.Fatalf("ConfigPath 应在 %s 下, got %q", dir, ConfigPath())
	}
	if err := EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("EnsureDir 后目录应存在: %v", err)
	}
}

func TestCurrentUserNonEmpty(t *testing.T) {
	if currentUser() == "" {
		t.Fatal("无法解析当前用户,user 缺省逻辑会失效")
	}
}
