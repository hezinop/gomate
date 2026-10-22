//go:build windows

package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// AppName 是开机自启动注册表里的键名，也用作通知的来源标识。
const AppName = "GoMate"

// 用 HKCU 而不是 HKLM：HKLM 需要管理员权限，而这是个人自用的程序，
// 不该为了「开机自启」这个小事去要 UAC 授权。
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// EnableAutoStart 把当前可执行文件写入 HKCU 的 Run 键，实现开机自启。
//
// exe 用 os.Executable() 推断，而不是让调用方传：
// 调用方很容易传成相对路径或没加引号的路径，开机时工作目录不确定会直接失败。
func EnableAutoStart(exeOverride string, args ...string) error {
	exe := exeOverride
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return fmt.Errorf("定位可执行文件失败: %w", err)
		}
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}

	// 路径含空格很常见（用户目录就常带空格），必须加引号，
	// 否则 Windows 会把 C:\Users\陈晓伟\... 从空格处截断成两个参数。
	cmd := `"` + exe + `"`
	if len(args) > 0 {
		cmd += " " + strings.Join(args, " ")
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 Run 键失败: %w", err)
	}
	defer k.Close()

	if err := k.SetStringValue(AppName, cmd); err != nil {
		return fmt.Errorf("写入开机自启失败: %w", err)
	}
	return nil
}

// DisableAutoStart 取消开机自启。已禁用时返回 nil（幂等）。
func DisableAutoStart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 Run 键失败: %w", err)
	}
	defer k.Close()

	if err := k.DeleteValue(AppName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除开机自启失败: %w", err)
	}
	return nil
}

// AutoStartStatus 返回开机自启的当前状态与注册的命令。
func AutoStartStatus() (enabled bool, cmd string, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.READ)
	if err != nil {
		return false, "", fmt.Errorf("打开 Run 键失败: %w", err)
	}
	defer k.Close()

	cmd, _, err = k.GetStringValue(AppName)
	if err == registry.ErrNotExist {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("读取开机自启失败: %w", err)
	}
	return true, cmd, nil
}
