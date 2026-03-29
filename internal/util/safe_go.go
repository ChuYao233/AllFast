package util

import (
	"log"
	"runtime/debug"
)

// SafeGo 启动一个带 panic 恢复的 goroutine，防止 panic 崩溃整个服务
func SafeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SafeGo] goroutine panic 已恢复: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}
