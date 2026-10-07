// Package exec 实现单机 exec 与多机 batch:exec channel 收 stdout/stderr/exit code,
// batch 用 errgroup + semaphore 并发、逐机超时与结果聚合。
//
// TODO(M1): 单机 exec。TODO(M2): batch 并发与聚合。可参考 GoSSHa 的 executeCmd(README §8)。
package exec
