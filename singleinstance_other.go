//go:build !windows

package desktop

// AcquireSingleInstance 在非 Windows 平台恒返回 true（不做单实例限制）。
func AcquireSingleInstance() bool { return true }

// ReleaseSingleInstance 在非 Windows 平台为空操作。
func ReleaseSingleInstance() {}
