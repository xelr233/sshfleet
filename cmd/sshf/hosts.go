package main

import "github.com/spf13/cobra"

func newHostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "管理 hosts.yaml 主机清单",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "add <name>",
			Short: "添加主机(交互式补全 address/user/凭据)",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("hosts add"),
		},
		&cobra.Command{
			Use:   "list",
			Short: "列出主机与分组",
			RunE:  notImplemented("hosts list"),
		},
		&cobra.Command{
			Use:   "rm <name>",
			Short: "删除主机",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("hosts rm"),
		},
		&cobra.Command{
			Use:   "test [name|group]...",
			Short: "对清单逐一做连通性检查",
			RunE:  notImplemented("hosts test"),
		},
	)
	return cmd
}
