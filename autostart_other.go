//go:build !windows

package desktop

// AppName 是开机自启动的注册名。
const AppName = "GoMate"

// EnableAutoStart 非 Windows 平台暂不支持。
//
// macOS 要写 ~/Library/LaunchAgents 的 plist，Linux 要写 ~/.config/autostart 的
// .desktop 文件，两者都要处理权限与格式差异。这里刻意不做半吊子实现 ——
// 明确返回 ErrUnsupported，让调用方能给用户一个诚实的提示。
func EnableAutoStart(exeOverride string, args ...string) error {
	return ErrUnsupported
}

func DisableAutoStart() error {
	return ErrUnsupported
}

func AutoStartStatus() (bool, string, error) {
	return false, "", ErrUnsupported
}
