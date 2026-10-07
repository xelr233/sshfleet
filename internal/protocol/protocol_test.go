package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestNewRequestRoundTrip(t *testing.T) {
	want := ExecParams{Host: "web-1", Command: "uptime", Timeout: 5 * time.Second}
	req, err := NewRequest(7, CmdExec, want)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if req.ID != 7 || req.Cmd != CmdExec {
		t.Fatalf("id/cmd 错误: %+v", req)
	}

	var got ExecParams
	if err := req.Bind(&got); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got != want {
		t.Fatalf("往返不一致: got %+v want %+v", got, want)
	}
}

func TestNewRequestNilParamsOmitsField(t *testing.T) {
	req, err := NewRequest(1, CmdStatus, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if len(req.Params) != 0 {
		t.Fatalf("nil params 应省略, got %q", req.Params)
	}
	// 无参数时 Bind 必须是 no-op,不报错。
	if err := req.Bind(&StatusResult{}); err != nil {
		t.Fatalf("Bind 无参数应为 no-op: %v", err)
	}
}

func TestResponseOKRoundTrip(t *testing.T) {
	want := ExecResult{Host: "web-1", Stdout: "hi\n", ExitCode: 0}
	resp, err := OK(9, want)
	if err != nil {
		t.Fatalf("OK: %v", err)
	}
	if resp.Err() != nil {
		t.Fatalf("成功响应不应带错误: %v", resp.Err())
	}
	var got ExecResult
	if err := resp.Bind(&got); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got != want {
		t.Fatalf("往返不一致: got %+v want %+v", got, want)
	}
}

func TestResponseFailAndBindReturnsError(t *testing.T) {
	resp := Fail(3, CodeUpstream, "主机 %s 握手失败", "web-9")
	err := resp.Err()
	if err == nil {
		t.Fatal("Fail 响应应带错误")
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("错误类型应为 *Error, got %T", err)
	}
	if perr.Code != CodeUpstream {
		t.Fatalf("错误码: got %d want %d", perr.Code, CodeUpstream)
	}
	if !strings.Contains(perr.Message, "web-9") {
		t.Fatalf("错误消息未格式化: %q", perr.Message)
	}
	// 对失败响应 Bind 应直接返回该错误,而不是尝试解析结果。
	if err := resp.Bind(&ExecResult{}); !errors.As(err, &perr) {
		t.Fatalf("失败响应 Bind 应返回 *Error, got %v", err)
	}
}

func TestEventRoundTrip(t *testing.T) {
	ev, err := NewEvent(5, EventProgress, DistHostState{Host: "a", Offset: 42, Total: 100})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	var got DistHostState
	if err := ev.Bind(&got); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got.Offset != 42 || got.Total != 100 {
		t.Fatalf("往返不一致: %+v", got)
	}
	if _, err := NewEvent(1, "", nil); err == nil {
		t.Fatal("空事件名应报错")
	}
}

// codecPair 通过内存管道连接一对 Reader / Writer,模拟真实 JSON 行协议链路。
func codecPair(t *testing.T) (*Writer, *Reader) {
	t.Helper()
	var buf bytes.Buffer
	return NewWriter(&buf), NewReader(&buf)
}

func TestCodecRequestOverPipe(t *testing.T) {
	w, r := codecPair(t)
	req, _ := NewRequest(1, CmdBatch, BatchParams{
		Sel:     HostSel{Hosts: []string{"a", "b"}, Group: "web"},
		Command: "df -h",
	})
	if err := w.WriteRequest(req); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got, err := r.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if got.ID != 1 || got.Cmd != CmdBatch {
		t.Fatalf("id/cmd 错误: %+v", got)
	}
	var p BatchParams
	if err := got.Bind(&p); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(p.Sel.Hosts) != 2 || p.Sel.Group != "web" {
		t.Fatalf("参数丢失: %+v", p)
	}
}

// 多条消息顺序写入后必须按序读回,且响应与事件可区分。
func TestCodecReplyDistinguishesEventAndResponse(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	resp, _ := OK(1, ExecResult{Host: "a"})
	ev, _ := NewEvent(2, EventTailLine, map[string]string{"line": "log"})
	if err := w.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if err := w.WriteResponse(resp); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	r := NewReader(&buf)
	gotResp, gotEv, err := r.ReadReply()
	if err != nil {
		t.Fatalf("ReadReply #1: %v", err)
	}
	if gotEv == nil || gotEv.Event != EventTailLine || gotEv.ID != 2 {
		t.Fatalf("第一条应为事件, got ev=%+v resp=%+v", gotEv, gotResp)
	}
	gotResp, gotEv, err = r.ReadReply()
	if err != nil {
		t.Fatalf("ReadReply #2: %v", err)
	}
	if gotResp == nil || gotResp.ID != 1 || gotEv != nil {
		t.Fatalf("第二条应为响应, got resp=%+v ev=%+v", gotResp, gotEv)
	}
	if _, _, err := r.ReadReply(); err != io.EOF {
		t.Fatalf("流末尾应为 io.EOF, got %v", err)
	}
}

// 空行必须被跳过,不能被当成消息。
func TestCodecSkipsBlankLines(t *testing.T) {
	r := NewReader(strings.NewReader("\n\n{\"id\":4,\"cmd\":\"status\"}\n"))
	req, err := r.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.ID != 4 || req.Cmd != CmdStatus {
		t.Fatalf("空行后消息解析错误: %+v", req)
	}
}

// stdout 中的 <、>、& 不应被 HTML 转义(SetEscapeHTML(false) 的回归保护)。
func TestCodecNoHTMLEscaping(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	resp, _ := OK(1, ExecResult{Stdout: "a < b && c > d\n"})
	if err := w.WriteResponse(resp); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`\u003c`)) {
		t.Fatalf("输出被 HTML 转义: %s", buf.String())
	}
}

// 无效 JSON 必须返回可读错误而非 panic。
func TestCodecInvalidJSON(t *testing.T) {
	r := NewReader(strings.NewReader("not json\n"))
	if _, err := r.ReadRequest(); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// 超过 MaxLineSize 的消息必须被拒绝。
func TestCodecRejectsOversizedLine(t *testing.T) {
	big := strings.Repeat("x", MaxLineSize+1024) + "\n"
	r := NewReader(strings.NewReader(big))
	if _, err := r.ReadRequest(); err == nil {
		t.Fatal("超限消息应报错")
	}
}
