package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.0s"},
		{-3 * time.Second, "0.0s"},
		{400 * time.Microsecond, "0.4ms"},
		{1234 * time.Microsecond, "1.2ms"},
		{999 * time.Millisecond, "999.0ms"},
		{1 * time.Second, "1.0s"},
		{1260 * time.Millisecond, "1.3s"},
		{3440 * time.Millisecond, "3.4s"},
		// 取整后跨档:不能被写成 "60.0s" / "60.0m"。
		{59970 * time.Millisecond, "1m0.0s"},
		{61250 * time.Millisecond, "1m1.3s"},
		{59 * time.Minute, "59m0.0s"},
		{time.Hour, "1h0m0.0s"},
		{3661240 * time.Millisecond, "1h1m1.2s"},
		{26 * time.Hour, "26h0m0.0s"},
	}
	for _, c := range cases {
		if got := Duration(c.in); got != c.want {
			t.Errorf("Duration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 小数点后最多 1 位,且不得出现 duration.String() 那种长尾(1.234567891s)。
func TestDurationAtMostOneDecimal(t *testing.T) {
	vals := []time.Duration{
		1234567 * time.Nanosecond,
		987654321 * time.Nanosecond,
		12345678901 * time.Nanosecond,
		99999999999 * time.Nanosecond,
	}
	for _, d := range vals {
		got := Duration(d)
		digits := 0
		seenDot := false
		for _, r := range got {
			switch {
			case r == '.':
				seenDot = true
			case r >= '0' && r <= '9' && seenDot:
				digits++
			default:
				seenDot = false
			}
		}
		if digits > 1 {
			t.Errorf("Duration(%v) = %q,小数位 %d 位", d, got, digits)
		}
	}
}
