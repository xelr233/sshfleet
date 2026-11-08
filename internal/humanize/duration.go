// Package humanize 提供面向终端展示的格式化函数。
package humanize

import (
	"fmt"
	"time"
)

// Duration 把耗时渲染成便于阅读的文本,统一最多保留 1 位小数。
//
//	412.3ms   不足 1 秒,以毫秒为单位
//	3.4s      不足 1 分钟
//	2m3.4s    不足 1 小时
//	1h2m3.4s  1 小时以上
//
// 先按 0.1 秒(不足 1 秒时按 0.1 毫秒)取整再挑量级,避免 59.97s 被写成
// "60.0s" 这种跨档结果。零值与负值统一渲染为 "0.0s"。
func Duration(d time.Duration) string {
	if d <= 0 {
		return "0.0s"
	}
	if d = d.Round(100 * time.Microsecond); d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	}
	d = d.Round(100 * time.Millisecond)
	sec := d / time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", float64(d)/float64(time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%.1fs", sec/60, float64(d%time.Minute)/float64(time.Second))
	default:
		return fmt.Sprintf("%dh%dm%.1fs",
			sec/3600, (sec%3600)/60, float64(d%time.Minute)/float64(time.Second))
	}
}
