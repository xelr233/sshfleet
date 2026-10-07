// Package tail 实现远端 tail -f 流式跟随:长驻通道 + 多客户端订阅 + 环形缓冲。
//
// TODO(M4): 尚未实现。客户端断开重连不得丢已推事件(环形缓冲,README §9 M4 验收)。
package tail
