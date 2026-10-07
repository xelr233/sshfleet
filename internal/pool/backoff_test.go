package pool

import (
	"testing"
	"time"
)

func TestNextBackoffDoubles(t *testing.T) {
	max := 30 * time.Second
	if got := nextBackoff(100*time.Millisecond, max); got != 200*time.Millisecond {
		t.Fatalf("应在 100ms 基础上翻倍, got %s", got)
	}
	if got := nextBackoff(0, max); got != 200*time.Millisecond {
		t.Fatalf("起始值 0 应取默认 100ms 再翻倍, got %s", got)
	}
}

func TestNextBackoffCapsAtMax(t *testing.T) {
	max := 30 * time.Second
	if got := nextBackoff(20*time.Second, max); got != max {
		t.Fatalf("接近上限时应封顶到 %s, got %s", max, got)
	}
	if got := nextBackoff(time.Duration(1)<<62, max); got != max {
		t.Fatalf("溢出时应封顶, got %s", got)
	}
}

// 从 100ms 起翻倍,应恰好在 9 次内触及 30s 上限并保持(README §5.8)。
func TestNextBackoffSequence(t *testing.T) {
	max := 30 * time.Second
	d := 100 * time.Millisecond
	want := []time.Duration{
		200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
		1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond,
		12800 * time.Millisecond, 25600 * time.Millisecond, 30 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		d = nextBackoff(d, max)
		if d != w {
			t.Fatalf("第 %d 次: got %s want %s", i+1, d, w)
		}
	}
}
