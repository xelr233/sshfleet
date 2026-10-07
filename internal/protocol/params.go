package protocol

import "time"

// 本文件定义各命令的参数与结果类型。它们是 daemon 与客户端的完整契约,
// 参数类型与 CmdXxx 同名加 Params 后缀,结果类型加 Result 后缀。

// HostSel 是一组主机选择条件:显式主机名列表,或一个 group(二者可并用,取并集)。
type HostSel struct {
	Hosts []string `json:"hosts,omitempty"`
	Group string   `json:"group,omitempty"`
}

// ---- status ----

// StatusResult 是 status 命令的结果。
type StatusResult struct {
	Version string        `json:"version"`
	Uptime  string        `json:"uptime"`
	Pool    []PoolHost    `json:"pool"`
	Tasks   []TaskSummary `json:"tasks"`
}

// PoolHost 描述一台主机的池内连接状态。
type PoolHost struct {
	Host       string    `json:"host"`
	Connected  bool      `json:"connected"`
	Channels   int       `json:"channels"`  // 当前占用的 channel 数
	DialedAt   time.Time `json:"dialed_at"` // 本连接的建立时间
	Reconnects int       `json:"reconnects"`
}

// TaskSummary 描述一个活动任务(batch / distribute 等)。
type TaskSummary struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// ---- exec ----

// ExecParams 是 exec 命令的参数。
type ExecParams struct {
	Host    string        `json:"host"`
	Command string        `json:"command"`
	Timeout time.Duration `json:"timeout,omitempty"` // 0 = 用 daemon 默认
}

// ExecResult 是单机执行结果。
type ExecResult struct {
	Host     string `json:"host"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Duration string `json:"duration"`
	Err      string `json:"err,omitempty"` // 连接/通道层错误时非空(区别于命令自身的非零退出码)
}

// ---- batch ----

// BatchParams 是 batch 命令的参数。
type BatchParams struct {
	Sel         HostSel       `json:"sel"`
	Command     string        `json:"command"`
	Concurrency int           `json:"concurrency,omitempty"` // 0 = 默认 5
	Timeout     time.Duration `json:"timeout,omitempty"`
}

// BatchResult 是多机执行结果:逐机结果数组 + 汇总。
type BatchResult struct {
	Results []ExecResult `json:"results"`
	Total   int          `json:"total"`
	OK      int          `json:"ok"`
	Failed  int          `json:"failed"`
	Elapsed string       `json:"elapsed"`
}

// ---- distribute ----

// DistStartParams 是 dist_start 命令的参数。
type DistStartParams struct {
	Sel         HostSel `json:"sel"`
	LocalPath   string  `json:"local_path"`
	RemotePath  string  `json:"remote_path"`
	Concurrency int     `json:"concurrency,omitempty"`
	Resume      bool    `json:"resume,omitempty"`
}

// DistStartResult 返回分发的任务 id。
type DistStartResult struct {
	TaskID string `json:"task_id"`
	Total  int    `json:"total"`
}

// DistStatusParams 是 dist_status / dist_cancel 的参数。
type DistStatusParams struct {
	TaskID string `json:"task_id"`
}

// DistStatusResult 是逐机进度。
type DistStatusResult struct {
	TaskID string          `json:"task_id"`
	State  string          `json:"state"` // running / done / canceled
	Hosts  []DistHostState `json:"hosts"`
	Done   int             `json:"done"`
	Failed int             `json:"failed"`
}

// DistHostState 是单机的分发状态。offset / total 支持断点续传。
type DistHostState struct {
	Host   string `json:"host"`
	State  string `json:"state"` // pending / running / done / failed
	Offset int64  `json:"offset"`
	Total  int64  `json:"total"`
	Err    string `json:"err,omitempty"`
}

// ---- sftp ----

// SFTPParams 是 sftp_put / sftp_get 的参数。
type SFTPParams struct {
	Host   string `json:"host"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// SFTPResult 是传输结果。
type SFTPResult struct {
	Host     string `json:"host"`
	Bytes    int64  `json:"bytes"`
	Duration string `json:"duration"`
	Err      string `json:"err,omitempty"`
}

// ---- session ----

// SessionOpenParams 是 session_open 的参数。
type SessionOpenParams struct {
	Host string `json:"host"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// SessionOpenResult 返回新会话 id。
type SessionOpenResult struct {
	SessionID string `json:"session_id"`
}

// SessionSendParams 是 session_send 的参数。
type SessionSendParams struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
}

// SessionCloseParams 是 session_close 的参数。
type SessionCloseParams struct {
	SessionID string `json:"session_id"`
}

// SessionListResult 是 session_list 的结果。
type SessionListResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionInfo 描述一个活动会话。
type SessionInfo struct {
	SessionID string    `json:"session_id"`
	Host      string    `json:"host"`
	OpenedAt  time.Time `json:"opened_at"`
	Idle      string    `json:"idle"`
}

// ---- tail ----

// TailStartParams 是 tail_start 的参数。
type TailStartParams struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

// TailStartResult 返回 tail 任务 id。
type TailStartResult struct {
	TaskID string `json:"task_id"`
}

// TailStopParams 是 tail_stop 的参数。
type TailStopParams struct {
	TaskID string `json:"task_id"`
}
