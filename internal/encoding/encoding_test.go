package encoding

import (
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func TestToUTF8PassesThroughValidUTF8(t *testing.T) {
	in := "hello 世界\n"
	if got := ToUTF8([]byte(in)); got != in {
		t.Fatalf("合法 UTF-8 应原样返回, got %q", got)
	}
}

func TestToUTF8DecodesGBK(t *testing.T) {
	want := "中文输出"
	gbk, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(want))
	if err != nil {
		t.Fatal(err)
	}
	// 清确保 GBK 编码确实不是合法 UTF-8(否则本测试无意义)。
	if got := ToUTF8(gbk); got != want {
		t.Fatalf("GBK 应解码为 %q, got %q", want, got)
	}
}

func TestToUTF8EmptyAndASCII(t *testing.T) {
	if got := ToUTF8(nil); got != "" {
		t.Fatalf("空输入应返回空串, got %q", got)
	}
	if got := ToUTF8([]byte("plain ascii")); got != "plain ascii" {
		t.Fatalf("ASCII 应原样返回, got %q", got)
	}
}
