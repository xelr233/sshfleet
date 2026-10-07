package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/daemon"
	"github.com/xelr233/sshfleet/internal/protocol"
)

func newDistributeCmd() *cobra.Command {
	var (
		hosts       []string
		group       string
		concurrency int
		resume      bool
	)
	cmd := &cobra.Command{
		Use:   "distribute <local> <remote> [--host h]... [--group g]",
		Short: "单文件向多机并发分发(磁盘只读一次,慢机不阻塞快机)",
		Long: `把本地文件分发到多台目标机。

目标通过 --host(可多次)与/或 --group 指定;也可用三参数写法
distribute <local> <host> <remote> 指定单台。

分发是 daemon 中的长任务:命令会持续查询进度并在结束时给出逐机汇总。
失败机器可用 --resume 从已传 offset 续传。`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			local := args[0]
			remote := args[len(args)-1]
			if len(args) == 3 {
				// 兼容 distribute <local> <host> <remote> 写法。
				hosts = append(hosts, args[1])
			}
			if len(hosts) == 0 && group == "" {
				return fmt.Errorf("distribute: 需指定 --host 或 --group(或用 <local> <host> <remote> 三参数写法)")
			}

			c, err := ensureDaemon(cmd)
			if err != nil {
				return err
			}
			defer c.Close()

			ctx := cmd.Context()
			resp, err := c.Do(ctx, protocol.CmdDistStart, protocol.DistStartParams{
				Sel:         protocol.HostSel{Hosts: hosts, Group: group},
				LocalPath:   local,
				RemotePath:  remote,
				Concurrency: concurrency,
				Resume:      resume,
			})
			if err != nil {
				return err
			}
			if err := resp.Err(); err != nil {
				return err
			}
			var start protocol.DistStartResult
			if err := resp.Bind(&start); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "分发任务 %s 已启动,共 %d 台目标\n", start.TaskID, start.Total)

			final, err := watchDist(ctx, c, start.TaskID, cmd)
			if err != nil {
				return err
			}
			return printDistSummary(cmd, final)
		},
	}
	cmd.Flags().StringSliceVar(&hosts, "host", nil, "目标主机(可多次 / 逗号分隔)")
	cmd.Flags().StringVar(&group, "group", "", "目标分组")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "并发度(0 = 默认 16)")
	cmd.Flags().BoolVar(&resume, "resume", false, "对已有远端文件从 offset 续传")
	return cmd
}

// watchDist 轮询 dist_status 直到任务结束,期间在状态变化时打印逐机进展。
func watchDist(ctx context.Context, c *daemon.Client, taskID string, cmd *cobra.Command) (*protocol.DistStatusResult, error) {
	seen := map[string]string{} // host → 已打印的状态,避免重复刷屏
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	for {
		resp, err := c.Do(ctx, protocol.CmdDistStatus, protocol.DistStatusParams{TaskID: taskID})
		if err != nil {
			return nil, err
		}
		if err := resp.Err(); err != nil {
			return nil, err
		}
		var st protocol.DistStatusResult
		if err := resp.Bind(&st); err != nil {
			return nil, err
		}
		printDistTransitions(cmd, &st, seen)

		if st.State != "running" {
			return &st, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// printDistTransitions 只打印状态发生变化的机器(含进度百分比),避免刷屏。
func printDistTransitions(cmd *cobra.Command, st *protocol.DistStatusResult, seen map[string]string) {
	out := cmd.OutOrStdout()
	for _, h := range st.Hosts {
		key := h.State
		if h.State == "running" {
			// running 用整体完成百分比作为 key,便于展示推进但不过密。
			key = fmt.Sprintf("running:%d", percent(h.Offset, h.Total)/10)
		}
		if seen[h.Host] == key {
			continue
		}
		seen[h.Host] = key
		fmt.Fprintf(out, "  %-16s %s\n", h.Host, describeDistHost(h))
	}
}

// describeDistHost 人读地描述一台机器的分发状态。
func describeDistHost(h protocol.DistHostState) string {
	switch h.State {
	case "done":
		return fmt.Sprintf("完成 %s", humanBytes(h.Offset))
	case "failed":
		return fmt.Sprintf("失败(已传 %s/%s): %s", humanBytes(h.Offset), humanBytes(h.Total), h.Err)
	case "running":
		return fmt.Sprintf("传输中 %s/%s (%d%%)", humanBytes(h.Offset), humanBytes(h.Total), percent(h.Offset, h.Total))
	default:
		return "等待中"
	}
}

func printDistSummary(cmd *cobra.Command, st *protocol.DistStatusResult) error {
	var failed []string
	for _, h := range st.Hosts {
		if h.State == "failed" || h.State == "pending" {
			failed = append(failed, h.Host)
		}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\n=== 汇总 ===\n")
	fmt.Fprintf(out, "总计 %d 台,完成 %d,失败 %d,状态 %s\n", len(st.Hosts), st.Done, st.Failed, st.State)
	if len(failed) > 0 {
		fmt.Fprintf(out, "未完成机器: %s\n", strings.Join(failed, ", "))
		fmt.Fprint(out, "可用 `sshf distribute ... --resume` 续传\n")
		return &exitCodeError{code: 1}
	}
	return nil
}

func percent(off, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(off * 100 / total)
}

// humanBytes 把字节数格式化为可读的 MiB/KiB 形式。
func humanBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
