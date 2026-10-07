package protocol

// Command 是请求的命令名。与 README §6 协议命令集 v1 一一对应。
// CLI 子命令与 MCP 工具都只映射到这里,不在各自层实现任何 SSH 逻辑。
type Command string

const (
	CmdStatus Command = "status" // daemon 版本、连接池状态、活动任务

	CmdExec  Command = "exec"  // 单机执行
	CmdBatch Command = "batch" // 多机并发执行

	CmdHostsTest Command = "hosts_test" // 逐机连通性检查(README §7 hosts test)

	CmdDistStart  Command = "dist_start"  // 分发,返回 task_id
	CmdDistStatus Command = "dist_status" // 逐机进度
	CmdDistCancel Command = "dist_cancel" // 取消分发

	CmdSFTPPut Command = "sftp_put" // 单机上传
	CmdSFTPGet Command = "sftp_get" // 单机下载

	CmdSessionOpen  Command = "session_open"
	CmdSessionSend  Command = "session_send"
	CmdSessionClose Command = "session_close"
	CmdSessionList  Command = "session_list"

	CmdTailStart Command = "tail_start"
	CmdTailStop  Command = "tail_stop"
)

// 事件名。服务端通过同连接推送,见 README §5.2。
const (
	EventOutput     = "output"      // session 输出增量
	EventProgress   = "progress"    // sftp / distribute 进度
	EventTailLine   = "tail_line"   // tail 到一行
	EventDistDone   = "dist_done"   // distribute 单机完成/失败
	EventSessionEnd = "session_end" // 会话结束(EOF 或远端退出)
)
