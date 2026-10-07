package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxLineSize 限制单条 JSON 消息的字节数,防止对端(或自身缺陷)导致内存无界增长。
// 取值需容纳最大的 exec stdout / sftp 结果;超限的消息会被拒绝并断开连接。
const MaxLineSize = 64 << 20 // 64 MiB

// Event 是服务端向客户端推送的异步消息(进度、输出、tail 行等)。
// 以请求 id 关联到发起它的任务;不占用响应槽位,见 README §5.2。
type Event struct {
	ID    uint64          `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// NewEvent 组装事件并把 data 编码为 JSON。
func NewEvent(id uint64, name string, data any) (*Event, error) {
	if name == "" {
		return nil, errors.New("protocol: 事件名不能为空")
	}
	e := &Event{ID: id, Event: name}
	if data == nil {
		return e, nil
	}
	b, err := marshal(data)
	if err != nil {
		return nil, fmt.Errorf("protocol: 编码事件 %s: %w", name, err)
	}
	e.Data = b
	return e, nil
}

// Bind 把事件数据解析到 v。
func (e *Event) Bind(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("protocol: 解析事件 %s: %w", e.Event, err)
	}
	return nil
}

// Writer 串行写 JSON 行。不是并发安全的:daemon 必须用单个写 goroutine
// 串行化所有写回(见 README §8 jsonReplierThread 的对应设计)。
type Writer struct {
	w   *bufio.Writer
	buf bytes.Buffer // 复用编码缓冲,避免每条消息一次分配
}

// NewWriter 包装 w。调用方需在使用后 Flush。
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriter(w)}
}

// Flush 把缓冲写入底层 writer。
func (w *Writer) Flush() error { return w.w.Flush() }

func (w *Writer) write(v any) error {
	w.buf.Reset()
	enc := json.NewEncoder(&w.buf)
	enc.SetEscapeHTML(false)              // stdout/stderr 内容不应被 HTML 转义
	if err := enc.Encode(v); err != nil { // Encode 自带结尾 '\n'
		return fmt.Errorf("protocol: 编码消息: %w", err)
	}
	if _, err := w.w.Write(w.buf.Bytes()); err != nil {
		return fmt.Errorf("protocol: 写消息: %w", err)
	}
	return nil
}

// WriteRequest 写一条请求。
func (w *Writer) WriteRequest(r *Request) error { return w.write(r) }

// WriteResponse 写一条响应。
func (w *Writer) WriteResponse(r *Response) error { return w.write(r) }

// WriteEvent 写一条事件。
func (w *Writer) WriteEvent(e *Event) error { return w.write(e) }

// Reader 读 JSON 行。
type Reader struct {
	r *bufio.Reader
}

// NewReader 包装 r。
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10)}
}

// readLine 读一条完整 JSON 行并去掉换行;超过 MaxLineSize 时报错。
func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			line = append(line, chunk...)
			if len(line) > MaxLineSize {
				return nil, fmt.Errorf("protocol: 消息超过 %d 字节上限", MaxLineSize)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		break
	}
	return bytes.TrimRight(line, "\r\n"), nil
}

// ReadRequest 读一条请求。返回值可能是 io.EOF。空行被忽略。
func (r *Reader) ReadRequest() (*Request, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return nil, fmt.Errorf("protocol: 解析请求: %w", err)
		}
		return &req, nil
	}
}

// ReadReply 读一条响应或事件,二者恰有一个非 nil。返回值可能是 io.EOF。
// 事件名非空即视为事件,否则视为响应(协议保证 Event 名永不为空)。
func (r *Reader) ReadReply() (*Response, *Event, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, nil, err
		}
		if len(line) == 0 {
			continue
		}
		var probe struct {
			ID     uint64          `json:"id"`
			Event  string          `json:"event"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, nil, fmt.Errorf("protocol: 解析响应: %w", err)
		}
		if probe.Event != "" {
			var ev Event
			if err := json.Unmarshal(line, &ev); err != nil {
				return nil, nil, fmt.Errorf("protocol: 解析事件: %w", err)
			}
			return nil, &ev, nil
		}
		return &Response{ID: probe.ID, Error: probe.Error, Result: probe.Result}, nil, nil
	}
}
