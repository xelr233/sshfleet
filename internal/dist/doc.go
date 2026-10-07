// Package dist 实现 distribute:io.Pipe 扇出、逐机进度、offset 断点续传、失败重试。
//
// TODO(M3): 本项目核心难点,尚未实现。磁盘只读一次 → chunk 广播到每机独立 io.Pipe,
// 每机 goroutine 从 pipe 读并 SFTP 顺序写单文件(勿并发写同一文件,README §5.4/§10.3)。
package dist
