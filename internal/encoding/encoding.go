// Package encoding 处理远端输出的字符编码。
//
// 目标机可能是 Windows(尤其 cmd.exe / 部分中文环境的 PowerShell),其输出常为
// GBK/CP936 而非 UTF-8。协议层与 CLI 都按 UTF-8 处理,因此需要在源头做一次
// 「UTF-8 优先、GBK 兜底」的解码(README §5.9 / §9 M6)。
//
// 无第三方依赖:GBK→UTF-8 用 Go 标准库的 golang.org/x/text 会引入依赖,这里按
// 「先判断是否合法 UTF-8,不合法再按 GBK 解」的策略,GBK 解码表用 x/text。
package encoding

import (
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// ToUTF8 把远端原始输出解码为 UTF-8。
//
// 策略:若字节已是合法 UTF-8,原样返回(绝大多数 Linux 目标);否则按 GBK 解码。
// 解码失败(非法 GBK 序列)时退回原字节,绝不因编码问题丢掉输出。
func ToUTF8(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	out, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), b)
	if err != nil {
		return string(b)
	}
	return string(out)
}
