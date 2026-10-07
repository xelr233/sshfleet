// Package sshconfig 解析 OpenSSH 客户端配置(~/.ssh/config)的一个子集,
// 用于把已有主机批量导入 hosts.yaml(CLI:`sshf hosts import`)。
//
// 只实现导入所需的语义:Host 块、HostName/User/Port/IdentityFile,以及 OpenSSH
// 的「首个取值生效(first obtained value wins)」规则。ProxyJump/ProxyCommand、
// Include、Match 等 sshf 暂不支持的能力会被记录为告警而不参与导入。
package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Entry 是一个具体别名(alias)解析后的连接参数。
type Entry struct {
	Alias        string // Host 行里的别名(导入后作为 hosts.yaml 的主机名)
	HostName     string // 为空时按 OpenSSH 语义用 Alias 充当地址
	User         string
	Port         int
	IdentityFile string
	// Unsupported 是该别名上出现、但 sshf 尚不支持的指令(如 proxyjump)。
	Unsupported []string
}

// Config 是一次解析的完整结果。
type Config struct {
	Entries  []Entry
	Warnings []string // 跳过的通配别名、Include、Match 等提示
}

// DefaultPath 返回默认的 OpenSSH 客户端配置路径(~/.ssh/config)。
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("sshconfig: 无法确定用户主目录: %w", err)
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// Load 读取并解析配置文件;path 为空时用 DefaultPath。
func Load(path string) (*Config, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sshconfig: 读取 %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("sshconfig: 解析 %s: %w", path, err)
	}
	return cfg, nil
}

// directive 是配置中的一条 `Key value`。
type directive struct {
	key   string // 已小写
	value string
	line  int
}

// block 是一组指令:要么是 Host 块(有 patterns),要么是 Match 块,要么是文件开头的全局块。
type block struct {
	global   bool     // 文件开头的全局默认块(作用于所有别名)
	patterns []string // Host 行里的模式
	match    bool     // Match 块(条件无法评估,不参与导入)
	dirs     []directive
	line     int
}

// Parse 解析 ssh config 文本。
func Parse(data []byte) (*Config, error) {
	blocks, err := splitBlocks(data)
	if err != nil {
		return nil, err
	}
	return resolve(blocks), nil
}

// splitBlocks 按 Host / Match 行把文件切成块,并收集全局块(首个 Host 之前的指令)。
func splitBlocks(data []byte) ([]block, error) {
	var blocks []block
	cur := &block{global: true}
	lineNo := 0

	for _, raw := range strings.Split(string(data), "\n") {
		lineNo++
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := splitDirective(line)
		if !ok {
			return nil, fmt.Errorf("第 %d 行无法解析: %q", lineNo, raw)
		}
		switch key {
		case "host", "match":
			// 切换块前,先把当前块落盘;开头的全局块只有含指令时才保留。
			if !cur.global || len(cur.dirs) > 0 {
				blocks = append(blocks, *cur)
			}
			if key == "host" {
				cur = &block{patterns: strings.Fields(value), line: lineNo}
			} else {
				cur = &block{match: true, line: lineNo}
			}
		default:
			cur.dirs = append(cur.dirs, directive{key: key, value: value, line: lineNo})
		}
	}
	if !cur.global || len(cur.dirs) > 0 {
		blocks = append(blocks, *cur)
	}
	return blocks, nil
}

// splitDirective 拆出 `Key value` 或 `Key=value`。key 统一小写。
func splitDirective(line string) (key, value string, ok bool) {
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), "", true // 形如 `ForwardAgent`(布尔,值为空)
	}
	key = strings.ToLower(line[:i])
	value = strings.TrimLeft(line[i:], " \t=")
	return key, strings.TrimSpace(value), key != ""
}

// resolve 按 OpenSSH「首个取值生效」规则,把各块合并成每个具体别名的参数。
func resolve(blocks []block) *Config {
	cfg := &Config{}

	// 仅接受具体别名(不含通配/否定),保持出现顺序。
	seen := map[string]bool{}
	var aliases []string
	for _, b := range blocks {
		if b.match {
			continue
		}
		for _, p := range b.patterns {
			if isConcrete(p) {
				if !seen[p] {
					seen[p] = true
					aliases = append(aliases, p)
				}
			} else if !strings.HasPrefix(p, "!") {
				// 通配别名无法导入,记为告警(去重)。
				cfg.warnOnce(fmt.Sprintf("跳过通配 Host %q(无法导入通配别名)", p))
			}
		}
	}
	for _, b := range blocks {
		if b.match {
			cfg.warnOnce(fmt.Sprintf("忽略 Match 段(第 %d 行):条件无法评估", b.line))
		}
		for _, d := range b.dirs {
			if d.key == "include" {
				cfg.warnOnce(fmt.Sprintf("忽略 Include(第 %d 行):不支持包含其它文件", d.line))
			}
		}
	}

	type acc struct {
		hostname, user, ident string
		port                  int
		unsupported           []string
	}
	accs := make(map[string]*acc, len(aliases))
	for _, a := range aliases {
		accs[a] = &acc{}
	}

	patCache := map[string]*regexp.Regexp{}
	for _, b := range blocks {
		if b.match {
			continue
		}
		for _, alias := range aliases {
			if !b.appliesTo(alias, patCache) {
				continue
			}
			a := accs[alias]
			for _, d := range b.dirs {
				switch d.key {
				case "hostname":
					if a.hostname == "" {
						a.hostname = d.value
					}
				case "user":
					if a.user == "" {
						a.user = d.value
					}
				case "port":
					if a.port == 0 {
						if n, err := strconv.Atoi(d.value); err == nil && n > 0 {
							a.port = n
						} else {
							cfg.warnOnce(fmt.Sprintf("第 %d 行的 Port %q 非法,已忽略", d.line, d.value))
						}
					}
				case "identityfile":
					if a.ident == "" && !strings.EqualFold(d.value, "none") {
						a.ident = unquote(d.value)
					}
				case "proxyjump", "proxycommand":
					a.unsupported = appendUnique(a.unsupported, d.key)
				}
			}
		}
	}

	for _, alias := range aliases {
		a := accs[alias]
		e := Entry{
			Alias:        alias,
			HostName:     a.hostname,
			User:         a.user,
			Port:         a.port,
			IdentityFile: a.ident,
			Unsupported:  a.unsupported,
		}
		if len(e.Unsupported) > 0 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"别名 %q 使用了 sshf 不支持的 %s,导入后请自行处理连接方式",
				alias, strings.Join(e.Unsupported, "/")))
		}
		cfg.Entries = append(cfg.Entries, e)
	}
	return cfg
}

// appliesTo 判断块是否作用于某具体别名。全局块作用于所有别名;
// 否定模式(!x)优先于肯定模式(OpenSSH 语义)。
func (b *block) appliesTo(alias string, cache map[string]*regexp.Regexp) bool {
	if b.match {
		return false
	}
	if b.global { // 文件开头的全局块
		return true
	}
	positive := false
	for _, p := range b.patterns {
		if strings.HasPrefix(p, "!") {
			if globMatch(p[1:], alias, cache) {
				return false
			}
			continue
		}
		if globMatch(p, alias, cache) {
			positive = true
		}
	}
	return positive
}

// isConcrete 判断模式是否为可直接导入的具体别名(无通配、非否定)。
func isConcrete(p string) bool {
	if p == "" || strings.HasPrefix(p, "!") {
		return false
	}
	return !strings.ContainsAny(p, "*?[")
}

// globMatch 用 OpenSSH 风格通配(* 与 ?)匹配别名。
func globMatch(pattern, s string, cache map[string]*regexp.Regexp) bool {
	if pattern == s {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return false
	}
	re, ok := cache[pattern]
	if !ok {
		re = regexp.MustCompile("^" + globToRegexp(pattern) + "$")
		cache[pattern] = re
	}
	return re.MatchString(s)
}

// globToRegexp 把 `*`/`?` 通配转成正则;其余字符转义,确保只按字面匹配。
func globToRegexp(pattern string) string {
	var sb strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return sb.String()
}

// unquote 去掉值两端的单/双引号(ssh config 允许带引号的文件路径)。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// warnOnce 去重追加告警。
func (c *Config) warnOnce(msg string) {
	for _, w := range c.Warnings {
		if w == msg {
			return
		}
	}
	c.Warnings = append(c.Warnings, msg)
}
