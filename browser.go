package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// OpenAppWindow 打开一个「像原生 App」的独立窗口。
//
// 关键在浏览器的 --app=URL 参数：它会开一个**没有地址栏、没有标签页、
// 没有书签栏**的窗口，视觉上与原生应用几乎无差别，任务栏也是独立图标。
//
// 为什么不直接内嵌 WebView（Wails/Tauri）：那需要 CGO，而且会把前端通信
// 从 HTTP+SSE 改成框架私有的 binding/事件机制 —— 现有的流式输出、断点续传、
// seq 补发全部要重写。用 --app 模式则前后端一行都不用改。
//
// 代价是依赖系统装有 Edge 或 Chrome（Windows 10/11 必定自带 Edge）。
func OpenAppWindow(url string) error {
	exe, err := findBrowser()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--app="+url, "--window-size=1120,860")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	return nil
}

// findBrowser 按优先级找出可用的浏览器可执行文件。
//
// 刻意用「已知安装路径」而不是查 PATH：GUI 程序的 PATH 在开机自启时
// 往往不完整（尤其通过注册表 Run 键拉起时），查 PATH 会随机失败。
func findBrowser() (string, error) {
	var candidates []string
	if runtime.GOOS == "windows" {
		candidates = []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
	} else {
		candidates = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/microsoft-edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("没找到可用的浏览器（Edge / Chrome），请手动打开 %s", "本机服务地址")
}
