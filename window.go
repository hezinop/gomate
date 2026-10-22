package desktop

// WindowConfig 描述一个原生窗口。
//
// 为什么不直接用系统浏览器（Edge --app=）：那样任务栏弹出的其实是浏览器进程，
// 用户感知上仍是「用浏览器打开网页」，而不是「一个应用」。
// 原生窗口让 gomated 成为真正独立的桌面程序：自己创建窗口、自己绘制，
// 除 Windows 自带的 WebView2 运行时外不依赖任何外部程序。
type WindowConfig struct {
	Title   string
	Width   int
	Height  int
	Icon    []byte // .ico 字节流；为空则用系统默认图标
	DataPath string // WebView2 用户数据目录
	Debug   bool   // 开启后可右键菜单 / DevTools，排障用
	// OnClose 在用户点击窗口关闭按钮时触发。
	// 注意：默认行为是「隐藏到托盘」而不是退出——服务与系统通知必须继续活着。
	OnClose func()
}
