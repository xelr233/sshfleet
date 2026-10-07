package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// New 返回一个空配置,hosts/groups 已初始化,可直接 Add。
func New() *File {
	return &File{Hosts: map[string]*Host{}, Groups: map[string][]string{}}
}

// LoadOrEmpty 加载配置;文件不存在时返回一个空配置(而非错误),
// 便于首次运行时直接 `hosts add` 而不必先手工创建文件。
func LoadOrEmpty(path string) (*File, error) {
	f, err := Load(path)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		empty := New()
		empty.path = path
		return empty, nil
	}
	return nil, err
}

// Save 把配置写回文件。写入是原子的(同目录临时文件 + rename),
// 避免进程中断留下半截 yaml。文件权限收紧到 0600(含明文密码时尤其重要)。
//
// 注意:yaml 重新序列化会丢失原文件中的注释与键顺序。需要在手写注释的
// hosts.yaml 上反复增删主机时,这会破坏人工维护的格式——属已知限制。
// 路径为空时写回加载来源。
func (f *File) Save(path string) error {
	if path == "" {
		path = f.path
	}
	if path == "" {
		return fmt.Errorf("config: 未指定保存路径")
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("config: 序列化: %w", err)
	}
	return writeFileAtomic(path, out, 0o600)
}

// writeFileAtomic 原子写文件:先写同目录临时文件,再 rename 覆盖。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: 创建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".hosts-*.tmp")
	if err != nil {
		return fmt.Errorf("config: 创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此调用为 no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: 写临时文件: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("config: 设置权限: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: 替换 %s: %w", path, err)
	}
	return nil
}
