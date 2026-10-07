// Package protocol 定义 sshfleet daemon 与客户端(CLI / MCP)之间的 JSON 行协议。
//
// 线格式:每条消息一行 JSON(以 '\n' 结尾),消息类型由字段区分:
//
//	请求  {"id":1,"cmd":"exec","params":{...}}
//	响应  {"id":1,"result":{...}}  或  {"id":1,"error":{"code":500,"message":"..."}}
//	事件  {"id":1,"event":"progress","data":{...}}
//
// id 由客户端自增分配,响应用同一 id 回带以便关联(同一连接可并发多个请求)。
// 流式 / 长任务(tail、distribute 进度、session 输出)不占用响应槽位,
// 而是通过同连接上的 event 消息推送,以请求 id 关联到发起它的任务。
//
// 本包是 daemon 与客户端共用的唯一契约:两侧都只能引用这里的类型,
// 不允许各自复制一份结构体定义。
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// marshal 编码 v 并去掉结尾换行,不转义 HTML 字符。
// shell 输出里 <、>、& 极常见(如 2>&1),转义会平白放大 payload,
// 因此本包所有编码路径统一关闭 HTML 转义。
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Request 是一条请求。Cmd 决定 Params 的解析类型(见各 XxxParams)。
type Request struct {
	ID     uint64          `json:"id"`
	Cmd    Command         `json:"cmd"`
	Params json.RawMessage `json:"params,omitempty"`
}

// NewRequest 组装请求并把 params 编码为 JSON。params 为 nil 时省略 params 字段。
func NewRequest(id uint64, cmd Command, params any) (*Request, error) {
	r := &Request{ID: id, Cmd: cmd}
	if params == nil {
		return r, nil
	}
	b, err := marshal(params)
	if err != nil {
		return nil, fmt.Errorf("protocol: 编码 %s 参数: %w", cmd, err)
	}
	r.Params = b
	return r, nil
}

// Bind 把请求参数解析到 v。无参数时不做任何事(便于服务端直接对 nil params 调 Bind)。
func (r *Request) Bind(v any) error {
	if len(r.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Params, v); err != nil {
		return fmt.Errorf("protocol: 解析 %s 参数: %w", r.Cmd, err)
	}
	return nil
}

// Response 是一条响应。Error 与 Result 至多有一个非空。
type Response struct {
	ID     uint64          `json:"id"`
	Error  *Error          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// OK 组装成功响应并把 result 编码为 JSON。result 为 nil 时省略 result 字段。
func OK(id uint64, result any) (*Response, error) {
	r := &Response{ID: id}
	if result == nil {
		return r, nil
	}
	b, err := marshal(result)
	if err != nil {
		return nil, fmt.Errorf("protocol: 编码结果: %w", err)
	}
	r.Result = b
	return r, nil
}

// Fail 组装失败响应。
func Fail(id uint64, code int, format string, args ...any) *Response {
	return &Response{ID: id, Error: &Error{Code: code, Message: fmt.Sprintf(format, args...)}}
}

// Bind 把响应结果解析到 v。响应本身失败时返回 *Error。
func (r *Response) Bind(v any) error {
	if r.Error != nil {
		return r.Error
	}
	if len(r.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Result, v); err != nil {
		return fmt.Errorf("protocol: 解析结果: %w", err)
	}
	return nil
}

// Err 返回响应的错误(没有错误时为 nil)。
func (r *Response) Err() error {
	if r.Error == nil {
		return nil
	}
	return r.Error
}

// Error 是协议层的错误对象,code 沿用 HTTP 语义便于人读。
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// 错误码。沿用 HTTP 状态语义,客户端可据此决定重试与否。
const (
	CodeBadRequest int = 400 // 参数非法(缺字段、解析失败)
	CodeNotFound   int = 404 // 主机 / 会话 / 任务不存在
	CodeTimeout    int = 408 // 单机或整体超时
	CodeUpstream   int = 502 // SSH 层失败(握手、通道、SFTP)
	CodeInternal   int = 500 // daemon 内部错误
)
