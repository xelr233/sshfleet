// Package daemon 提供 daemon 主循环,以及供 CLI 使用的客户端。
//
// 客户端逻辑(连接、鉴权、发请求)放在本包的 client.go:它与服务端共用 protocol
// 契约,且需要复用 state.go 中的端口 / token 文件读取,放在同一包最自然。
package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/xelr233/sshfleet/internal/protocol"
)

// Client 是与 daemon 的一条连接。非并发安全:每次请求用独立 Client 或自行串行化。
type Client struct {
	conn   net.Conn
	reader *protocol.Reader
	writer *protocol.Writer
}

// Dial 连接正在运行的 daemon:读取端口与 token 文件、建立 TCP 连接并完成 token 握手。
func Dial() (*Client, error) {
	port, err := ReadPort()
	if err != nil {
		return nil, err
	}
	token, err := ReadToken()
	if err != nil {
		return nil, err
	}
	return DialAddr(port, token)
}

// DialAddr 连接指定的地址并鉴权。主要供测试使用。
func DialAddr(port int, token string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon: 连接 daemon(127.0.0.1:%d): %w", port, err)
	}
	c := &Client{
		conn:   conn,
		reader: protocol.NewReader(conn),
		writer: protocol.NewWriter(conn),
	}
	if err := c.handshake(token); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake(token string) error {
	if _, err := c.conn.Write([]byte(token + "\n")); err != nil {
		return fmt.Errorf("daemon: 发送 token: %w", err)
	}
	return nil
}

// Close 关闭连接。
func (c *Client) Close() error { return c.conn.Close() }

// Do 发送一条请求并等待其响应(忽略途中收到的事件)。
func (c *Client) Do(ctx context.Context, cmd protocol.Command, params any) (*protocol.Response, error) {
	req, err := protocol.NewRequest(1, cmd, params)
	if err != nil {
		return nil, err
	}
	if err := c.writer.WriteRequest(req); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}

	// 用 goroutine 读,便于响应 ctx 取消。
	type result struct {
		resp *protocol.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			resp, ev, err := c.reader.ReadReply()
			if err != nil {
				ch <- result{nil, err}
				return
			}
			if ev != nil {
				continue // 简单请求场景忽略事件(M2/M3 会改为分发处理)
			}
			ch <- result{resp, nil}
			return
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.resp, r.err
	}
}

// IsRunning 报告是否已有 daemon 在运行(仅检查 PID 文件中的进程是否存活)。
func IsRunning() bool {
	pid, err := ReadPID()
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}
