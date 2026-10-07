// Package sshtest 提供一个进程内的最小 SSH 服务器,仅用于测试。
//
// 它让 pool / exec / daemon 的测试不依赖开发机上真实的 sshd,也不依赖网络,
// 从而可以稳定地回归「连接复用」「断线重连」「channel 限流」等关键行为。
// 本包不是产品代码,不应被 cmd/ 或非测试代码导入。
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// User / Password 是测试服务器接受的固定凭据。
const (
	User     = "testuser"
	Password = "testpass"
)

// Server 是进程内 SSH 服务器。
type Server struct {
	ln   net.Listener
	cfg  *ssh.ServerConfig
	addr string

	handshakes atomic.Int64 // 成功完成的 SSH 握手次数(用于断言连接复用)
	mu         sync.Mutex
	conns      []net.Conn
	closed     atomic.Bool
	wg         sync.WaitGroup
}

// Start 在 127.0.0.1 的随机空闲端口启动服务器。
func Start() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return startWith(ln)
}

// StartAt 在指定监听地址启动服务器(用于重连测试:同一地址重启)。
func StartAt(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return startWith(ln)
}

func startWith(ln net.Listener) (*Server, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == User && string(p) == Password {
				return nil, nil
			}
			return nil, fmt.Errorf("sshtest: 认证失败")
		},
	}
	cfg.AddHostKey(signer)

	s := &Server{ln: ln, cfg: cfg, addr: ln.Addr().String()}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回 host:port。
func (s *Server) Addr() string { return s.addr }

// Host / Port 拆分返回,便于填充 config.Host。
func (s *Server) Host() string {
	h, _, _ := net.SplitHostPort(s.addr)
	return h
}

// Port 返回监听端口。
func (s *Server) Port() int {
	_, p, _ := net.SplitHostPort(s.addr)
	n, _ := strconv.Atoi(p)
	return n
}

// Handshakes 返回成功握手次数。
func (s *Server) Handshakes() int64 { return s.handshakes.Load() }

// Close 停止服务器并断开所有活动连接。
func (s *Server) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(c)
		}()
	}
}

func (s *Server) handleConn(nc net.Conn) {
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		nc.Close()
		return
	}
	s.handshakes.Add(1)
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "仅支持 session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleSession(ch, chReqs)
		}()
	}
	sconn.Close()
}

// handleSession 实现最小 exec 语义,足以驱动 pool / exec 的测试:
//
//	"exit N"      → 退出码 N
//	"stderr"      → stdout "out" + stderr "err"
//	"sleep"       → 保持会话不返回(用于测试超时 / channel 占用)
//	其它          → 原样回显到 stdout,退出码 0
func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			cmd := parseCommand(req.Payload)
			req.Reply(true, nil)
			switch {
			case strings.HasPrefix(cmd, "exit "):
				code, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(cmd, "exit ")))
				writeExitStatus(ch, code)
			case cmd == "stderr":
				ch.Write([]byte("out\n"))
				ch.Stderr().Write([]byte("err\n"))
				writeExitStatus(ch, 0)
			case cmd == "sleep":
				// 不回复退出状态,让客户端阻塞(由测试主动取消)。
				<-chanDone(reqs)
				return
			default:
				ch.Write([]byte("echo:" + cmd + "\n"))
				writeExitStatus(ch, 0)
			}
			ch.Close()
			return
		case "shell":
			req.Reply(true, nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// chanDone 在请求通道关闭(会话结束)时返回。
func chanDone(reqs <-chan *ssh.Request) chan struct{} {
	done := make(chan struct{})
	go func() {
		for range reqs {
		}
		close(done)
	}()
	return done
}

// parseCommand 从 exec 请求负载中取出命令行(前 4 字节为长度)。
func parseCommand(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if int(n) > len(payload)-4 {
		n = uint32(len(payload) - 4)
	}
	return string(payload[4 : 4+n])
}

// writeExitStatus 发送 exit-status 请求。
func writeExitStatus(ch ssh.Channel, code int) {
	payload := ssh.Marshal(struct{ Status uint32 }{uint32(code)})
	ch.SendRequest("exit-status", false, payload)
}
