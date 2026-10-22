//go:build !windows

package desktop

// 非 Windows 平台没有 WebView2，原生窗口不可用。
// 保留这套空实现是为了让 cmd/gomated 在各平台都能编译，
// 运行时会自动回退到浏览器窗口模式。

// WebView2Ready 在非 Windows 平台恒为 false。
func WebView2Ready() bool { return false }

// Window 是非 Windows 平台的原生窗口占位类型。
type Window struct{}

// NewWindow 在非 Windows 平台永远返回 nil（调用方走回退逻辑）。
func NewWindow(cfg WindowConfig) *Window { return nil }

// Run 是非 Windows 平台的消息循环占位。
func (w *Window) Run() {}

// NavigateTo 在非 Windows 平台为空操作。
func (w *Window) NavigateTo(url string) {}

// Show 在非 Windows 平台为空操作。
func (w *Window) Show() {}

// Quit 在非 Windows 平台为空操作。
func (w *Window) Quit() {}

// Dispatch 在非 Windows 平台为空操作。
func (w *Window) Dispatch(f func()) {}
