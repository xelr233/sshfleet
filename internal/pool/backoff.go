package pool

import "time"

// nextBackoff 计算下一次重连退避时长:翻倍,但不超过 max。
// 起始值由 Options.BackoffBase 决定(默认 100ms),封顶 Options.BackoffMax(默认 30s),
// 对应 README §5.8。
func nextBackoff(cur, max time.Duration) time.Duration {
	if cur <= 0 {
		cur = 100 * time.Millisecond
	}
	n := cur * 2
	if n <= 0 || n > max { // 防止溢出或超过上限
		return max
	}
	return n
}
