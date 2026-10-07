package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/sshconfig"
)

// importRow 记录一条待导入主机在预览表里的状态。
type importRow struct {
	alias  string
	addr   string
	user   string
	port   int
	key    string
	status string // 新增 / 更新 / 跳过 / 失败
	err    error
}

func newHostsImportCmd() *cobra.Command {
	var (
		file      string
		group     string
		overwrite bool
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "从 OpenSSH 配置(~/.ssh/config)导入主机",
		Long: `解析 OpenSSH 客户端配置,把其中具体别名导入 hosts.yaml。

支持的指令:Host / HostName / User / Port / IdentityFile。
不支持(会告警并跳过其效果):通配别名(Host *)、Include、Match、ProxyJump、ProxyCommand。

已有同名主机默认跳过,加 --overwrite 覆盖;--dry-run 只预览不落盘。`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHostsImport(cmd, file, group, overwrite, dryRun)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "OpenSSH 配置路径(默认 ~/.ssh/config)")
	cmd.Flags().StringVar(&group, "group", "", "把导入的主机加入该分组")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "覆盖 hosts.yaml 中已存在的同名主机")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "只预览,不写入 hosts.yaml")
	return cmd
}

func runHostsImport(cmd *cobra.Command, file, group string, overwrite, dryRun bool) error {
	cfg, err := sshconfig.Load(file)
	if err != nil {
		return err
	}
	// 告警(跳过的通配别名、Include/Match、不支持的指令)统一走 stderr。
	for _, w := range cfg.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "警告: %s\n", w)
	}
	if len(cfg.Entries) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "未解析到可导入的主机")
		return nil
	}

	f, err := loadConfig()
	if err != nil {
		return err
	}

	rows := make([]importRow, 0, len(cfg.Entries))
	added, updated, skipped, failed := 0, 0, 0, 0
	for _, e := range cfg.Entries {
		addr := e.HostName
		if addr == "" {
			addr = e.Alias // OpenSSH:未写 HostName 时用别名作为地址
		}
		h := &config.Host{
			Name: e.Alias, Addr: addr, User: e.User, Port: e.Port, Key: e.IdentityFile,
		}
		row := importRow{alias: e.Alias, addr: addr, user: e.User, port: e.Port, key: e.IdentityFile}

		exists := f.Hosts[e.Alias] != nil
		if exists && !overwrite {
			row.status = "跳过(已存在)"
			skipped++
			rows = append(rows, row)
			continue
		}
		if err := f.Add(h); err != nil {
			row.status = "失败"
			row.err = err
			failed++
			rows = append(rows, row)
			continue
		}
		if group != "" {
			if err := f.AddToGroup(group, e.Alias); err != nil {
				row.status = "失败"
				row.err = err
				failed++
				rows = append(rows, row)
				continue
			}
		}
		if exists {
			row.status = "更新"
			updated++
		} else {
			row.status = "新增"
			added++
		}
		rows = append(rows, row)
	}

	printImportTable(cmd, rows)

	out := cmd.OutOrStdout()
	if dryRun {
		fmt.Fprintf(out, "\n[dry-run] 新增 %d,更新 %d,跳过 %d,失败 %d(未写入)\n",
			added, updated, skipped, failed)
		return nil
	}
	if added+updated == 0 {
		fmt.Fprintf(out, "\n无变更(新增 0,更新 0,跳过 %d,失败 %d)\n", skipped, failed)
		return nil
	}
	if err := saveConfig(f); err != nil {
		return err
	}
	path, _ := resolveConfigPath()
	fmt.Fprintf(out, "\n已写入 %s(新增 %d,更新 %d,跳过 %d,失败 %d)\n",
		path, added, updated, skipped, failed)
	if failed > 0 {
		return &exitCodeError{code: 1}
	}
	return nil
}

// printImportTable 打印待导入主机的预览表。
func printImportTable(cmd *cobra.Command, rows []importRow) {
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tADDR\tUSER\tPORT\tKEY\tSTATUS")
	for _, r := range rows {
		port := "-"
		if r.port > 0 {
			port = fmt.Sprintf("%d", r.port)
		}
		status := r.status
		if r.err != nil {
			status = fmt.Sprintf("%s: %v", r.status, r.err)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.alias, r.addr, dashIfEmpty(r.user), port, dashIfEmpty(r.key), status)
	}
	w.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
