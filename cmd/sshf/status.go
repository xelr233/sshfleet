package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/xelr233/sshfleet/internal/protocol"
)

// printStatus 渲染 status 结果:版本/运行时长 + 连接池表 + 活动任务。
func printStatus(cmd *cobra.Command, st *protocol.StatusResult) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "sshm daemon  v%s   运行 %s\n", st.Version, st.Uptime)

	if len(st.Pool) == 0 {
		fmt.Fprintln(out, "\n连接池: 空(尚无主机被访问)")
	} else {
		fmt.Fprintln(out, "\n连接池:")
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  HOST\tSTATE\tCHANNELS\tRECONNECTS\tDIALED")
		for _, p := range st.Pool {
			state := "disconnected"
			if p.Connected {
				state = "connected"
			}
			dialed := "-"
			if p.Connected && !p.DialedAt.IsZero() {
				dialed = p.DialedAt.Format("15:04:05")
			}
			fmt.Fprintf(w, "  %s\t%s\t%d\t%d\t%s\n",
				p.Host, state, p.Channels, p.Reconnects, dialed)
		}
		w.Flush()
	}

	if len(st.Tasks) > 0 {
		fmt.Fprintln(out, "\n活动任务:")
		for _, t := range st.Tasks {
			fmt.Fprintf(out, "  %s [%s] %d/%d\n", t.ID, t.Kind, t.Done, t.Total)
		}
	}
}
