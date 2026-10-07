package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/xelr233/sshfleet/internal/config"
)

// genKey 生成一份 OpenSSH 私钥写入 dir,返回路径。passphrase 非空则加密。
func genKey(t *testing.T, dir, name, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 ed25519: %v", err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatalf("序列化私钥: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
	return p
}

func TestLoadSignerUnencrypted(t *testing.T) {
	p := genKey(t, t.TempDir(), "id_ed25519", "")
	signer, err := LoadSigner(p, "")
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	if signer.PublicKey().Type() != "ssh-ed25519" {
		t.Fatalf("公钥类型异常: %s", signer.PublicKey().Type())
	}
}

func TestLoadSignerEncryptedWithPassphrase(t *testing.T) {
	p := genKey(t, t.TempDir(), "id_ed25519", "s3cret")
	signer, err := LoadSigner(p, "s3cret")
	if err != nil {
		t.Fatalf("带口令加载失败: %v", err)
	}
	if signer == nil {
		t.Fatal("signer 为空")
	}
}

func TestLoadSignerEncryptedMissingPassphraseGivesReadableError(t *testing.T) {
	p := genKey(t, t.TempDir(), "id_ed25519", "s3cret")
	_, err := LoadSigner(p, "")
	if err == nil {
		t.Fatal("缺口令应报错")
	}
	if !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("错误应提示配置 passphrase, got: %v", err)
	}
}

func TestLoadSignerWrongPassphrase(t *testing.T) {
	p := genKey(t, t.TempDir(), "id_ed25519", "right")
	if _, err := LoadSigner(p, "wrong"); err == nil {
		t.Fatal("错误口令应报错")
	}
}

func TestLoadSignerMissingFile(t *testing.T) {
	if _, err := LoadSigner(filepath.Join(t.TempDir(), "nope"), ""); err == nil {
		t.Fatal("文件不存在应报错")
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无主目录")
	}
	got, err := ExpandPath("~/.ssh/id_ed25519")
	if err != nil {
		t.Fatalf("ExpandPath: %v", err)
	}
	want := filepath.Join(home, ".ssh", "id_ed25519")
	if got != want {
		t.Fatalf("展开错误: got %q want %q", got, want)
	}
	// 非 ~ 开头原样返回(相对路径由调用方负责解释)。
	rel := filepath.Join("keys", "a")
	if got, err := ExpandPath(rel); err != nil || got != rel {
		t.Fatalf("非 ~ 路径应原样返回: got %q err %v", got, err)
	}
	if _, err := ExpandPath(""); err == nil {
		t.Fatal("空路径应报错")
	}
}

func TestAuthMethodsPriority(t *testing.T) {
	p := genKey(t, t.TempDir(), "id_ed25519", "")
	h := &config.Host{Name: "x", User: "u", Addr: "h", Key: p, Password: "pw"}
	methods, warnings, err := AuthMethods(h)
	if err != nil {
		t.Fatalf("AuthMethods: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("不应有告警, got %v", warnings)
	}
	// key 在前、password 在后(agent 未启用)。
	if len(methods) != 2 {
		t.Fatalf("应有 2 个认证方式, got %d", len(methods))
	}
}

func TestAuthMethodsPasswordOnly(t *testing.T) {
	h := &config.Host{Name: "x", User: "u", Addr: "h", Password: "pw"}
	methods, _, err := AuthMethods(h)
	if err != nil {
		t.Fatalf("AuthMethods: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("应有 1 个认证方式, got %d", len(methods))
	}
}

func TestAuthMethodsNoCredentials(t *testing.T) {
	h := &config.Host{Name: "x", User: "u", Addr: "h"}
	if _, _, err := AuthMethods(h); err == nil {
		t.Fatal("无凭据应报错")
	}
}

// agent 不可用但同时配了密码时,应降级继续并给出告警,而不是整体失败(README §10.2)。
func TestAuthMethodsAgentDegradesToPassword(t *testing.T) {
	h := &config.Host{Name: "x", User: "u", Addr: "h", Agent: true, Password: "pw"}
	methods, warnings, err := AuthMethods(h)
	if err != nil {
		t.Fatalf("agent 不可用但密码可用时应降级成功: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("降级后应只剩密码认证, got %d", len(methods))
	}
	if len(warnings) != 1 {
		t.Fatalf("应给出 1 条降级告警, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "agent") {
		t.Fatalf("告警应提及 agent, got %q", warnings[0])
	}
}

// agent 是唯一凭据且不可用时,才应整体失败。
func TestAuthMethodsAgentOnlyUnavailableIsFatal(t *testing.T) {
	h := &config.Host{Name: "x", User: "u", Addr: "h", Agent: true}
	if _, _, err := AuthMethods(h); err == nil {
		t.Fatal("agent 是唯一凭据且不可用时应报错")
	}
}

func TestEnumerateKeys(t *testing.T) {
	dir := t.TempDir()
	genKey(t, dir, "id_ed25519", "")
	genKey(t, dir, "id_rsa", "")
	// 加密私钥也应是候选(需用户提供 passphrase)。
	genKey(t, dir, "id_ecdsa_personal", "pw") // 名字不命中候选,不应返回
	// 干扰文件:名字命中但不是私钥。
	if err := os.WriteFile(filepath.Join(dir, "id_dsa"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := EnumerateKeys(dir)
	want := []string{
		filepath.Join(dir, "id_ed25519"),
		filepath.Join(dir, "id_rsa"),
	}
	if len(got) != len(want) {
		t.Fatalf("候选数: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选顺序错误: got %v want %v", got, want)
		}
	}
}

func TestEnumerateKeysOnlyEncryptedKeyIsStillCandidate(t *testing.T) {
	dir := t.TempDir()
	enc := genKey(t, dir, "id_ed25519", "pw")
	got := EnumerateKeys(dir)
	if len(got) != 1 || got[0] != enc {
		t.Fatalf("加密私钥应作为候选返回, got %v", got)
	}
}

func TestAgentSocketPerPlatform(t *testing.T) {
	got := AgentSocket()
	if runtime.GOOS == "windows" {
		if got != WindowsAgentPipe {
			t.Fatalf("Windows 应返回命名管道, got %q", got)
		}
		return
	}
	// 非 Windows:随环境变量变化。
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	if AgentSocket() != "/tmp/agent.sock" {
		t.Fatalf("应读取 SSH_AUTH_SOCK, got %q", AgentSocket())
	}
}
