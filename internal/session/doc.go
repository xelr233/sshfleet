// Package session 实现长连接 shell 会话:channel 生命周期、cd/env 延续、
// 流式 stdout 增量解析与多客户端订阅。
//
// TODO(M4): 尚未实现。必须用 session.Shell() 持久 stdin/stdout,不能依赖 exec channel(README §5.6)。
package session
