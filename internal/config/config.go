// Package config 负责 hosts.yaml 的加载、校验与解析,以及 ~/.sshfleet/ 状态文件路径。
//
// 本包不依赖 protocol / pool 等上层包:它是整条链路最底层的纯数据层,
// 只做「文件 ↔ 结构体 ↔ 选择集解析」,不发起任何网络动作。
package config

import (
	"fmt"
	"os"
	"os/user"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Host 是一台目标机的连接配置。
//
// 凭据优先级(见 README §5.7):Agent > Key > Password。
// Key 为 OpenSSH 私钥路径;Password 为明文(加载时告警,后续支持密文)。
type Host struct {
	Name string `yaml:"-"` // 由 host map 的 key 填充,不写回文件

	Addr string `yaml:"addr"`           // host:port 中的 host(不含端口)
	User string `yaml:"user,omitempty"` // 缺省用当前 OS 用户
	Port int    `yaml:"port,omitempty"` // 缺省 22

	Key        string   `yaml:"key,omitempty"`        // 私钥路径
	Passphrase string   `yaml:"passphrase,omitempty"` // 私钥口令(可选)
	Password   string   `yaml:"password,omitempty"`   // 明文密码
	Agent      bool     `yaml:"agent,omitempty"`      // 允许走 ssh-agent
	Cipher     []string `yaml:"cipher,omitempty"`     // 协商优先级覆盖(空=默认)
}

// File 是 hosts.yaml 的根结构。
type File struct {
	// Defaults 的字段会填充到未显式设置该字段的主机上,减少重复。
	Defaults Host `yaml:"defaults,omitempty"`

	Hosts  map[string]*Host    `yaml:"hosts"`
	Groups map[string][]string `yaml:"groups,omitempty"`

	path string // 加载来源,保存时复用
}

// DefaultPort 是未指定端口时的默认 SSH 端口。
const DefaultPort = 22

// Load 从 path 读取并校验配置。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
	}
	return Parse(data, path)
}

// LoadDefault 加载默认路径(~/.sshfleet/hosts.yaml)的配置。
func LoadDefault() (*File, error) {
	return Load(ConfigPath())
}

// Parse 解析 yaml 并校验。
func Parse(data []byte, path string) (*File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("config: 解析 %s: %w", path, err)
	}
	f.path = path
	if f.Hosts == nil {
		f.Hosts = map[string]*Host{}
	}
	for name, h := range f.Hosts {
		if h == nil {
			h = &Host{}
			f.Hosts[name] = h
		}
		h.Name = name
		mergeDefaults(h, &f.Defaults)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// mergeDefaults 把 defaults 中已设置、而主机未设置的字段补上。
func mergeDefaults(h, d *Host) {
	if h.User == "" {
		h.User = d.User
	}
	if h.Port == 0 {
		h.Port = d.Port
	}
	if h.Key == "" {
		h.Key = d.Key
	}
	if h.Passphrase == "" {
		h.Passphrase = d.Passphrase
	}
	if h.Password == "" {
		h.Password = d.Password
	}
	if !h.Agent {
		h.Agent = d.Agent
	}
	if len(h.Cipher) == 0 {
		h.Cipher = d.Cipher
	}
}

// Validate 检查配置的完整性与引用一致性,并补齐隐式默认值。
func (f *File) Validate() error {
	for name, h := range f.Hosts {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("config: 存在空主机名")
		}
		if h.Addr == "" {
			return fmt.Errorf("config: 主机 %q 缺少 addr", name)
		}
		if h.Port == 0 {
			h.Port = DefaultPort
		}
		if h.Port < 1 || h.Port > 65535 {
			return fmt.Errorf("config: 主机 %q 端口 %d 越界", name, h.Port)
		}
		if h.User == "" {
			h.User = currentUser()
		}
		if h.User == "" {
			return fmt.Errorf("config: 主机 %q 未指定 user,且无法取当前用户", name)
		}
	}
	for group, members := range f.Groups {
		for _, m := range members {
			if _, ok := f.Hosts[m]; !ok {
				return fmt.Errorf("config: 分组 %q 引用了不存在的主机 %q", group, m)
			}
		}
	}
	return nil
}

// Resolve 把「显式主机列表 + 分组名」解析成主机集合并去重、按名排序。
// 两者可并用,结果为并集;都不为空时返回错误(调用方应至少给一个)。
// 至少给一个选择条件,否则报错,避免误操作全部主机。
func (f *File) Resolve(hosts []string, group string) ([]*Host, error) {
	if len(hosts) == 0 && group == "" {
		return nil, fmt.Errorf("config: 未指定主机或分组")
	}
	selected := map[string]*Host{}
	for _, name := range hosts {
		h, ok := f.Hosts[name]
		if !ok {
			return nil, fmt.Errorf("config: 未知主机 %q", name)
		}
		selected[name] = h
	}
	if group != "" {
		members, ok := f.Groups[group]
		if !ok {
			return nil, fmt.Errorf("config: 未知分组 %q", group)
		}
		for _, name := range members {
			selected[name] = f.Hosts[name]
		}
	}
	out := make([]*Host, 0, len(selected))
	for _, h := range selected {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Names 返回排序后的全部主机名,供 hosts list 等场景展示。
func (f *File) Names() []string {
	out := make([]string, 0, len(f.Hosts))
	for name := range f.Hosts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// GroupNames 返回排序后的全部分组名。
func (f *File) GroupNames() []string {
	out := make([]string, 0, len(f.Groups))
	for name := range f.Groups {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Add 新增或整体替换一台主机,并重新校验。
func (f *File) Add(h *Host) error {
	if h == nil || strings.TrimSpace(h.Name) == "" {
		return fmt.Errorf("config: 主机名不能为空")
	}
	f.Hosts[h.Name] = h
	mergeDefaults(h, &f.Defaults)
	if err := f.Validate(); err != nil {
		delete(f.Hosts, h.Name) // 校验失败回滚,不污染内存状态
		return err
	}
	return nil
}

// Remove 删除一台主机,并同步从所有分组中移除其引用。
func (f *File) Remove(name string) error {
	if _, ok := f.Hosts[name]; !ok {
		return fmt.Errorf("config: 未知主机 %q", name)
	}
	delete(f.Hosts, name)
	for g, members := range f.Groups {
		kept := members[:0]
		for _, m := range members {
			if m != name {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			delete(f.Groups, g)
			continue
		}
		f.Groups[g] = kept
	}
	return nil
}

// AddToGroup 把主机加入分组(幂等)。主机必须已存在。
func (f *File) AddToGroup(group, host string) error {
	if group == "" {
		return fmt.Errorf("config: 分组名不能为空")
	}
	if _, ok := f.Hosts[host]; !ok {
		return fmt.Errorf("config: 未知主机 %q", host)
	}
	if f.Groups == nil {
		f.Groups = map[string][]string{}
	}
	for _, m := range f.Groups[group] {
		if m == host {
			return nil // 已在组内
		}
	}
	f.Groups[group] = append(f.Groups[group], host)
	return nil
}

// PlaintextPasswordHosts 返回使用明文密码的主机名,供启动期安全告警(README §5.7)。
func (f *File) PlaintextPasswordHosts() []string {
	var out []string
	for _, name := range f.Names() {
		if f.Hosts[name].Password != "" {
			out = append(out, name)
		}
	}
	return out
}

// currentUser 返回当前进程所属用户的登录名,失败时返回空串。
func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		// Windows 返回 DOMAIN\user,只取登录名部分。
		if i := strings.LastIndexAny(u.Username, `\/`); i >= 0 {
			return u.Username[i+1:]
		}
		return u.Username
	}
	return os.Getenv("USER")
}
