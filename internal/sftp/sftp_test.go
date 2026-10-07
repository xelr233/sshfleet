package sftp

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelr233/sshfleet/internal/config"
	"github.com/xelr233/sshfleet/internal/pool"
	"github.com/xelr233/sshfleet/internal/sshtest"
)

// newSFTPClient 启动一个以 localRoot 为 SFTP 根目录的测试服务器,并返回一条会话。
func newSFTPClient(t *testing.T, localRoot string, thr *sshtest.Throttle) *Client {
	t.Helper()
	srv, err := sshtest.Start()
	if err != nil {
		t.Fatalf("启动测试 SSH 服务器: %v", err)
	}
	srv.Root = localRoot
	srv.Throttle = thr
	t.Cleanup(srv.Close)

	cfg := config.New()
	if err := cfg.Add(&config.Host{
		Name: "h1", Addr: srv.Host(), Port: srv.Port(),
		User: sshtest.User, Password: sshtest.Password,
	}); err != nil {
		t.Fatalf("主机配置: %v", err)
	}
	p := pool.New(cfg, pool.Options{})
	t.Cleanup(func() { p.Close() })

	conn, err := p.Get(context.Background(), "h1")
	if err != nil {
		t.Fatalf("取连接: %v", err)
	}
	c, err := Open(context.Background(), conn)
	if err != nil {
		t.Fatalf("打开 SFTP 会话: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPutAndGetRoundTrip(t *testing.T) {
	root := t.TempDir()
	c := newSFTPClient(t, root, nil)

	data := make([]byte, 3*ChunkSize+1234)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}

	var lastOffset, lastTotal int64
	n, err := c.Put(context.Background(), bytes.NewReader(data), "sub/dir/file.bin", PutOptions{
		Total:      int64(len(data)),
		Mkdir:      true,
		OnProgress: func(off, total int64) { lastOffset, lastTotal = off, total },
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("写入字节数应为 %d, got %d", len(data), n)
	}
	if lastOffset != int64(len(data)) || lastTotal != int64(len(data)) {
		t.Fatalf("最终进度应为 total,total: got %d,%d", lastOffset, lastTotal)
	}

	// 直接读磁盘校验(服务器 Root 即本地目录)。
	got, err := os.ReadFile(filepath.Join(root, "sub", "dir", "file.bin"))
	if err != nil {
		t.Fatalf("读回远端文件: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("上传内容不一致")
	}

	// 下载回环。
	var out bytes.Buffer
	dn, err := c.Get(context.Background(), &out, "sub/dir/file.bin", PutOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dn != int64(len(data)) {
		t.Fatalf("下载字节数应为 %d, got %d", len(data), dn)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("下载内容不一致")
	}
}

// RemoteSize 对不存在的文件返回 -1(而非错误),便于 resume 判断。
func TestRemoteSizeMissing(t *testing.T) {
	c := newSFTPClient(t, t.TempDir(), nil)
	sz, err := c.RemoteSize("nope.bin")
	if err != nil {
		t.Fatalf("RemoteSize: %v", err)
	}
	if sz != -1 {
		t.Fatalf("不存在文件应为 -1, got %d", sz)
	}
}

// 续传:先写入前半段,再以 Offset 续写后半段;最终内容必须完整且不重复。
func TestPutResumeFromOffset(t *testing.T) {
	root := t.TempDir()
	c := newSFTPClient(t, root, nil)

	data := make([]byte, 200*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	const half = 100 * 1024

	// 第一段
	if _, err := c.Put(context.Background(), bytes.NewReader(data[:half]), "f.bin", PutOptions{Total: int64(len(data))}); err != nil {
		t.Fatalf("第一段: %v", err)
	}
	// 模拟「远端比预期多写了 100 字节」,验证续传会先截断到 offset 再写。
	extra := filepath.Join(root, "f.bin")
	f, _ := os.OpenFile(extra, os.O_APPEND|os.O_WRONLY, 0o600)
	f.Write([]byte("XXXX"))
	f.Close()

	// 第二段:从 half 续写
	if _, err := c.Put(context.Background(), bytes.NewReader(data[half:]), "f.bin", PutOptions{
		Offset: half, Total: int64(len(data)),
	}); err != nil {
		t.Fatalf("续传段: %v", err)
	}

	got, err := os.ReadFile(extra)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("续传后内容不一致: len=%d want=%d", len(got), len(data))
	}
}

// ctx 取消应中止传输并返回错误,而不是把文件传完。
func TestPutContextCancel(t *testing.T) {
	root := t.TempDir()
	// 限速使传输足够慢,以便在途中取消。
	c := newSFTPClient(t, root, &sshtest.Throttle{BytesPerSec: 64 * 1024})

	data := make([]byte, 8<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := c.Put(ctx, bytes.NewReader(data), "big.bin", PutOptions{Total: int64(len(data))})
	if err == nil {
		t.Fatal("ctx 取消后 Put 应返回错误")
	}
}

// 读源出错时 Put 应把错误上抛(而非静默截断)。
func TestPutPropagatesReaderError(t *testing.T) {
	c := newSFTPClient(t, t.TempDir(), nil)
	sentinel := errors.New("读失败")
	r := io.MultiReader(bytes.NewReader(make([]byte, 1024)), errReader{sentinel})
	if _, err := c.Put(context.Background(), r, "x.bin", PutOptions{}); err == nil {
		t.Fatal("源读错误应上抛")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
