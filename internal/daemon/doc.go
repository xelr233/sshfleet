// Package daemon 实现 daemon 主循环:TCP 监听、token 鉴权、JSON 行协议路由、
// 空闲退出,以及 PID/端口/token 文件管理。
//
// TODO(M1): 尚未实现。按 README §5.1/§5.2 落地 127.0.0.1 TCP + 启动期随机 token,
// 并复用 protocol 包的 Reader/Writer 做 JSON 行编解码。
package daemon
