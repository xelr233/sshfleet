// Package pool 是 SSH 连接池:dial、keepalive、断线指数退避重连、singleflight
// 防惊群,以及每主机 channel 数 semaphore 限流。
//
// TODO(M1): 尚未实现。可参考 GoSSHa 的 connHostsMap/getConnection(README §8),
// 但必须补 keepalive 与 singleflight,且所有阻塞操作带 context。
package pool
