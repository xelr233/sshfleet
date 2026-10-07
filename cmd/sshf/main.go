package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "0.1.0"

func main() {
	// 顶层 context:收到 Ctrl-C / SIGTERM 时取消,daemon 前台模式据此优雅退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := newRootCmd().ExecuteContext(ctx)
	if err == nil {
		return
	}

	// 远端命令的非零退出码直接作为本进程退出码(便于脚本判断),不再打印错误。
	var ec *exitCodeError
	if errors.As(err, &ec) {
		os.Exit(ec.code)
	}

	// 打印前把 cobra/pflag 的英文报错本地化;此处已过 exitCodeError 分支,
	// 包装错误不会影响退出码判定。
	fmt.Fprintf(os.Stderr, "错误: %v\n", localizeError(err))
	os.Exit(1)
}
