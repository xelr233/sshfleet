package main

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/protocol"
)

func newHostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "管理 hosts.yaml 主机清单",
	}
	cmd.AddCommand(
		newHostsAddCmd(),
		newHostsListCmd(),
		newHostsRmCmd(),
		newHostsTestCmd(),
	)
	return cmd
}

func newHostsAddCmd() *cobra.Command {
	var (
		addr, user, key, passphrase, password string
		port                                  int
		agent                                 bool
		group                                 string
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "添加主机(缺失字段交互式补全)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			f, err := loadConfig()
			if err != nil {
				return err
			}

			// 缺失的必填字段才提示,已用标志给出的不打扰用户。
			if addr == "" {
				if addr, err = prompt("地址(host)"); err != nil {
					return err
				}
			}
			if user == "" {
				if user, err = prompt("用户"); err != nil {
					return err
				}
			}
			// 凭据:三者都没给时提示私钥路径(留空表示后续手动编辑)。
			if !agent && key == "" && password == "" {
				if key, err = prompt("私钥路径(可留空)"); err != nil {
					return err
				}
			}

			h := &config.Host{
				Name: name, Addr: addr, User: user, Port: port,
				Key: key, Passphrase: passphrase, Password: password, Agent: agent,
			}
			if err := f.Add(h); err != nil {
				return err
			}
			if group != "" {
				if err := f.AddToGroup(group, name); err != nil {
					return err
				}
			}
			if err := saveConfig(f); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "已添加主机 %s (%s@%s:%d, 凭据: %s)\n",
				name, h.User, h.Addr, h.Port, credentialKind(h))
			if h.Password != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "警告: 明文密码将落盘到 hosts.yaml,建议改用私钥或 ssh-agent(README §5.7)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "主机地址(不含端口)")
	cmd.Flags().StringVar(&user, "user", "", "登录用户")
	cmd.Flags().IntVar(&port, "port", 0, "SSH 端口(默认 22)")
	cmd.Flags().StringVar(&key, "key", "", "OpenSSH 私钥路径")
	cmd.Flags().StringVar(&passphrase, "passphrase", "", "私钥口令")
	cmd.Flags().StringVar(&password, "password", "", "密码(明文落盘,会告警)")
	cmd.Flags().BoolVar(&agent, "agent", false, "使用 ssh-agent")
	cmd.Flags().StringVar(&group, "group", "", "同时加入该分组")
	return cmd
}

func newHostsListCmd() *cobra.Command {
	var showPath bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出主机与分组",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath()
			if err != nil {
				return err
			}
			f, err := config.LoadOrEmpty(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if showPath {
				fmt.Fprintln(out, path)
				return nil
			}
			if len(f.Hosts) == 0 {
				fmt.Fprintf(out, "暂无主机。用 `sshf hosts add <name>` 添加(%s)\n", path)
				return nil
			}

			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUSER\tADDR\tPORT\tCREDENTIAL\tGROUPS")
			for _, name := range f.Names() {
				h := f.Hosts[name]
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
					name, h.User, h.Addr, h.Port, credentialKind(h), strings.Join(groupsOf(f, name), ","))
			}
			if err := w.Flush(); err != nil {
				return err
			}

			if len(f.Groups) > 0 {
				fmt.Fprintln(out, "\ngroups:")
				for _, g := range f.GroupNames() {
					fmt.Fprintf(out, "  %s: %s\n", g, strings.Join(f.Groups[g], ", "))
				}
			}
			if hosts := f.PlaintextPasswordHosts(); len(hosts) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "\n警告: 以下主机使用明文密码: %s\n", strings.Join(hosts, ", "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showPath, "path", false, "只打印配置文件路径")
	return cmd
}

func newHostsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "删除主机(同步清理分组引用)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			f, err := loadConfig()
			if err != nil {
				return err
			}
			if err := f.Remove(name); err != nil {
				return err
			}
			if err := saveConfig(f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除主机 %s\n", name)
			return nil
		},
	}
}

func newHostsTestCmd() *cobra.Command {
	var group string
	cmd := &cobra.Command{
		Use:   "test [name...]",
		Short: "对清单逐一做连通性检查(建连 + 认证)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && group == "" {
				return fmt.Errorf("hosts test: 需指定主机名或 --group")
			}
			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			resp, err := doReq(c, cmd, protocol.CmdHostsTest, protocol.HostsTestParams{
				Sel: protocol.HostSel{Hosts: args, Group: group},
			})
			if err != nil {
				return err
			}
			var res protocol.HostsTestResult
			if err := resp.Bind(&res); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			failed := 0
			for _, h := range res.Hosts {
				if h.OK {
					fmt.Fprintf(out, "%-16s OK     %s\n", h.Host, h.Latency)
				} else {
					failed++
					fmt.Fprintf(out, "%-16s FAIL   %s\n", h.Host, h.Err)
				}
			}
			if failed > 0 {
				fmt.Fprintf(out, "\n%d/%d 台不可达\n", failed, len(res.Hosts))
				return &exitCodeError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "目标分组")
	return cmd
}

// groupsOf 返回某主机所属的分组名(已排序)。
func groupsOf(f *config.File, host string) []string {
	var out []string
	for g, members := range f.Groups {
		for _, m := range members {
			if m == host {
				out = append(out, g)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
