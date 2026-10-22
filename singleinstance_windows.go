//go:build windows

package desktop

// 单实例保护。
//
// 为什么需要：桌面版常驻后台，用户完全可能「开机自启起了一个，又手动点了一个」。
// 两个实例会各起一套服务（端口自动避让后会变成两个端口）、两个托盘图标，
// 还会共享同一个 SQLite 库——提醒可能重复推送。用一个具名互斥体挡掉。

import (
	"golang.org/x/sys/windows"
)

// singleInstanceMutexName 是本应用专用的互斥体名。
// 用 Local\ 前缀：同一用户的不同 Windows 会话（如远程桌面）允许各跑一个。
const singleInstanceMutexName = `Local\GoMate-SingleInstance`

var instanceMutex windows.Handle

// AcquireSingleInstance 尝试获取单实例锁。
// 返回 false 表示已有一个 GoMate 在运行，调用方应直接退出。
func AcquireSingleInstance() bool {
	name, err := windows.UTF16PtrFromString(singleInstanceMutexName)
	if err != nil {
		return true // 构造名字都失败就别挡路了，放行
	}
	h, err := windows.CreateMutex(nil, false, name)
	// 注意：互斥体已存在时句柄依然是有效的，只是 err 为 ERROR_ALREADY_EXISTS，
	// 这正是「已有实例」的信号。
	if err == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return false
	}
	if err != nil || h == 0 {
		return true // 创建失败也放行，别因为锁本身不可用把程序卡死
	}
	// 句柄必须持有到进程结束：一关闭，锁就失效，第二个实例就能起来
	instanceMutex = h
	return true
}

// ReleaseSingleInstance 释放单实例锁（进程退出前调用）。
func ReleaseSingleInstance() {
	if instanceMutex != 0 {
		_ = windows.CloseHandle(instanceMutex)
		instanceMutex = 0
	}
}
