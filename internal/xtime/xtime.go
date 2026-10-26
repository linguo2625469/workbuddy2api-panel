// Package xtime 提供与时间相关的、被多个包共用的小工具。
//
// 为什么独立成包：sleepCtx 曾在 server 与 scheduler 各存一份逐字相同的实现
// （server 那份的注释还写着「按等价物口径在消费侧实现」）。两份可取消等待的实现
// 正是「其中一份悄悄丢掉 t.Stop() 而泄漏定时器」的温床，故抽为单一事实来源。
package xtime

import (
	"context"
	"time"
)

// SleepCtx 可取消的等待：ctx 取消立即返回 false（客户端断连 / 优雅停机不必等
// 退避睡醒），等满返回 true。d<=0 时立即放行（仅以 ctx 是否仍存活为结果）。
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
