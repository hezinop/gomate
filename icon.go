package desktop

import _ "embed"

// Icon 是系统托盘图标，32x32 的 .ico。
//
// Windows 托盘只接受 .ico，不接受 PNG——这是 Win32 API 的硬性要求，
// 不是 systray 库的限制。所以内嵌的是 ICO 而非更常见的 PNG。
//
// Windows 下用 go:embed 而不是运行时读文件，是因为程序要能单文件分发：
// 托盘图标如果依赖同目录的 assets 文件夹，用户一挪动 exe 图标就没了。
//
//go:embed assets/app.ico
var Icon []byte
