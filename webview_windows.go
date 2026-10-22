//go:build windows

package desktop

// 原生窗口实现（WebView2）。
//
// 这是 gomated 成为「独立应用」的关键：不再启动 Edge/Chrome 再套一个 --app=
// 窗口，而是用 Windows 自带的 WebView2 运行时自己创建窗口。
// 用户视角：任务栏上是 GoMate 自己的图标，进程列表里只有 gomated.exe，
// 没有任何浏览器参与。
//
// 技术要点：
//   - 纯 Go，无 CGO（本机 gcc 6.3.0 太老，Wails 那条路走不通）
//   - WebView2Loader.dll 由 go-webview2 从内存加载，单文件分发
//   - 关闭按钮被子类化的窗口过程拦截：隐藏到托盘而非退出，
//     这样「关掉窗口也能收提醒」才成立
//
// 线程模型（重要）：
//   - go-webview2 的 edge 包在 init() 里对【主协程】做 LockOSThread + STA COM
//     初始化，因此窗口必须在主线程创建、消息循环也必须在主线程跑
//   - 系统托盘跑在另一个自己 LockOSThread 的协程里，两个消息循环互不干扰
//   - 跨线程操作窗口一律走 Window.Dispatch（PostThreadMessage 到窗口线程）

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"unsafe"

	gowebview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ---------------------------------------------------------------- Win32 常量

// gwlpWndproc 用 int64 变量而不是常量：GWLP_WNDPROC 是 -4，
// 负常量直接转 uintptr 会编译报错（overflows），运行时转换则正常截断。
var gwlpWndproc = int64(-4) // GWLP_WNDPROC

const (
	wmClose        = 0x0010
	wmSetIcon      = 0x0080
	swHide         = 0
	swRestore      = 9
	iconSmall      = 0 // ICON_SMALL
	iconBig        = 1 // ICON_BIG
	imageIcon      = 1 // IMAGE_ICON
	lrLoadFromFile = 0x0010
	loadIconSize   = 32
)

var (
	user32                = windows.NewLazySystemDLL("user32")
	procSetWindowLongPtrW = user32.NewProc("SetWindowLongPtrW")
	procSetWindowLongW    = user32.NewProc("SetWindowLongW")
	procCallWindowProcW   = user32.NewProc("CallWindowProcW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procShowWindow        = user32.NewProc("ShowWindow")
	procSetForeground     = user32.NewProc("SetForegroundWindow")
	procLoadImageW        = user32.NewProc("LoadImageW")
	procSendMessageW      = user32.NewProc("SendMessageW")
	procPostMessageW      = user32.NewProc("PostMessageW")
)

// ---------------------------------------------------------------- 窗口过程子类化

// windowState 记录每个被子类化窗口的原始窗口过程和所属 Window。
type windowState struct {
	origProc uintptr
	win      *Window
}

var (
	wndStates  sync.Map // uintptr(hwnd) -> *windowState
	wndProcPtr = windows.NewCallback(wndProc)
)

// wndProc 是接管后的窗口过程。
//
// 默认的 WM_CLOSE 会直接销毁窗口并退出消息循环——那意味着用户点一下 X，
// 后台服务和提醒推送就全没了。这里把它改成「隐藏到托盘」；
// 只有在确认为退出流程（win.quitting）时才放行默认的销毁行为。
func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	v, ok := wndStates.Load(hwnd)
	if !ok {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}
	st := v.(*windowState)

	if msg == wmClose && !st.win.quitting.Load() {
		st.win.hideOnUIThread()
		if cb := st.win.cfg.OnClose; cb != nil {
			cb()
		}
		return 0 // 吞掉关闭：窗口只是藏起来，进程继续跑
	}

	r, _, _ := procCallWindowProcW.Call(st.origProc, hwnd, msg, wp, lp)
	return r
}

// setWndProc 安装子类化窗口过程，返回原过程地址。
// 32/64 位通用：SetWindowLongPtrW 只在 64 位 Windows 上存在。
func setWndProc(hwnd, proc uintptr) uintptr {
	if procSetWindowLongPtrW.Find() == nil {
		r, _, _ := procSetWindowLongPtrW.Call(hwnd, uintptr(gwlpWndproc), proc)
		return r
	}
	r, _, _ := procSetWindowLongW.Call(hwnd, uintptr(gwlpWndproc), proc)
	return r
}

// ---------------------------------------------------------------- Window

// Window 是一个原生 WebView2 窗口。
type Window struct {
	cfg      WindowConfig
	wv       gowebview.WebView
	hwnd     uintptr
	quitting atomic.Bool
	navigated atomic.Bool
}

// NewWindow 创建原生窗口。
//
// 必须在主线程调用（edge 包的 init 已把 COM STA 初始化绑在主线程）。
// 返回 nil 表示创建失败，调用方应回退到浏览器窗口模式。
func NewWindow(cfg WindowConfig) *Window {
	if cfg.Width <= 0 {
		cfg.Width = 1180
	}
	if cfg.Height <= 0 {
		cfg.Height = 860
	}

	w := &Window{cfg: cfg}
	w.wv = gowebview.NewWithOptions(gowebview.WebViewOptions{
		Debug:    cfg.Debug,
		DataPath: cfg.DataPath,
		AutoFocus: true,
		WindowOptions: gowebview.WindowOptions{
			Title:  cfg.Title,
			Width:  uint(cfg.Width),
			Height: uint(cfg.Height),
			Center: true,
		},
	})
	if w.wv == nil {
		return nil
	}
	w.hwnd = uintptr(w.wv.Window())
	if w.hwnd == 0 {
		return nil
	}

	// 先把关闭按钮改成「隐藏到托盘」，再干别的
	if orig := setWndProc(w.hwnd, wndProcPtr); orig != 0 {
		wndStates.Store(w.hwnd, &windowState{origProc: orig, win: w})
	} else {
		// 子类化失败不至于打不开窗口，只是点 X 会直接退出程序
		fmt.Println("警告: 窗口过程子类化失败，关闭窗口将直接退出")
	}

	// 先显示一个「启动中」页面：等服务就绪再导航到真正的界面，
	// 避免白屏或 WebView2 自带的「无法访问此页面」错误页
	w.wv.SetHtml(loadingHTML)

	w.setIcon()
	return w
}

// loadingHTML 是服务就绪前的占位页面。
const loadingHTML = `<!DOCTYPE html><html><head><meta charset="utf-8"></head>
<body style="margin:0;height:100vh;display:flex;align-items:center;justify-content:center;
background:#f6f7f9;font-family:system-ui,'Microsoft YaHei',sans-serif;color:#5b6472">
<div style="text-align:center">
<div style="width:34px;height:34px;margin:0 auto 14px;border-radius:50%;
background:#3b82f6;opacity:.85"></div>
<div style="font-size:14px;letter-spacing:.08em">GoMate 正在启动…</div>
</div></body></html>`

// Run 阻塞运行窗口消息循环，直到窗口被销毁。
// 必须在创建 Window 的同一个线程调用。
func (w *Window) Run() {
	if w == nil || w.wv == nil {
		return
	}
	w.wv.Run()
	wndStates.Delete(w.hwnd)
}

// NavigateTo 导航到指定地址并把窗口带到前台。可从任意线程调用。
func (w *Window) NavigateTo(url string) {
	if w == nil || w.wv == nil {
		return
	}
	w.Dispatch(func() {
		w.navigated.Store(true)
		w.wv.Navigate(url)
		w.showOnUIThread()
	})
}

// Show 把窗口带到前台（从托盘恢复）。可从任意线程调用。
func (w *Window) Show() {
	if w == nil || w.wv == nil {
		return
	}
	w.Dispatch(w.showOnUIThread)
}

// Quit 销毁窗口并结束消息循环。可从任意线程调用（托盘「退出」用它）。
func (w *Window) Quit() {
	if w == nil || w.hwnd == 0 {
		return
	}
	w.quitting.Store(true)
	// WM_CLOSE 排到窗口所在线程的消息队列，由那里的窗口过程执行销毁
	_, _, _ = procPostMessageW.Call(w.hwnd, wmClose, 0, 0)
}

// Dispatch 把函数排到窗口所在线程执行（跨线程操作窗口的唯一安全姿势）。
func (w *Window) Dispatch(f func()) {
	if w == nil || w.wv == nil {
		return
	}
	w.wv.Dispatch(f)
}

// hideOnUIThread 隐藏窗口。只能在窗口所在线程调用（窗口过程里）。
func (w *Window) hideOnUIThread() {
	if w == nil || w.hwnd == 0 {
		return
	}
	_, _, _ = procShowWindow.Call(w.hwnd, swHide)
}

func (w *Window) showOnUIThread() {
	if w == nil || w.hwnd == 0 {
		return
	}
	_, _, _ = procShowWindow.Call(w.hwnd, swRestore)
	_, _, _ = procSetForeground.Call(w.hwnd)
}

// setIcon 给窗口设置任务栏图标。
//
// Win32 的 LoadImage(LR_LOADFROMFILE) 只认磁盘文件，而程序要单文件分发，
// 所以把内嵌的 .ico 写到临时目录再加载（加载是同步的，加载完即可删）。
func (w *Window) setIcon() {
	if len(w.cfg.Icon) == 0 {
		return
	}
	path := filepath.Join(os.TempDir(), "gomate-window-icon.ico")
	if err := os.WriteFile(path, w.cfg.Icon, 0o644); err != nil {
		return
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	h, _, _ := procLoadImageW.Call(
		0,
		uintptr(unsafe.Pointer(p)),
		imageIcon,
		loadIconSize,
		loadIconSize,
		lrLoadFromFile,
	)
	if h == 0 {
		return
	}
	_, _, _ = procSendMessageW.Call(w.hwnd, wmSetIcon, iconSmall, h)
	_, _, _ = procSendMessageW.Call(w.hwnd, wmSetIcon, iconBig, h)
}

// ---------------------------------------------------------------- 运行时探测

// WebView2Ready 探测系统是否安装了 WebView2 运行时。
//
// 这个检查是必须的：go-webview2 在运行时缺失时会直接 log.Fatal 杀进程，
// 体验太差。探测失败就回退到浏览器窗口模式，并提示用户安装运行时。
func WebView2Ready() bool {
	// 最常见：evergreen 运行时的安装目录
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\EdgeWebView\Application`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\EdgeWebView\Application`),
	}
	for _, dir := range candidates {
		if dir == "" || dir == `Microsoft\EdgeWebView\Application` {
			continue
		}
		if hasVersionSubdir(dir) {
			return true
		}
	}
	// 注册表兜底
	const client = `SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	if registryHasPV(registry.CURRENT_USER, client) {
		return true
	}
	if registryHasPV(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`) {
		return true
	}
	return false
}

// hasVersionSubdir 判断目录下是否存在版本号形式的子目录（如 155.0.4283.60）。
func hasVersionSubdir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if name := e.Name(); name != "" && name[0] >= '0' && name[0] <= '9' {
			return true
		}
	}
	return false
}

// registryHasPV 判断 EdgeUpdate 客户端键下是否有非空的 pv（版本）值。
func registryHasPV(root registry.Key, path string) bool {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	pv, _, err := k.GetStringValue("pv")
	return err == nil && pv != ""
}
