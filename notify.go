// Package desktop 封装桌面端才有的三条系统能力。
//
// 为什么需要这个包：Web 版的死穴是「浏览器一关，主动推送就收不到了」，
// 而主动提醒恰恰是这个项目最核心的能力。系统通知 + 开机自启让提醒真正成立。
//
// 通知走 beeep（纯 Go，无 CGO）；开机自启与窗口打开是平台相关的，
// 按 GOOS 分文件实现，在其他系统上优雅退化（返回 ErrUnsupported）而非编译失败。
package desktop

import (
	"errors"
	"strings"

	"github.com/gen2brain/beeep"
)

// ErrUnsupported 表示当前平台不支持该能力。
var ErrUnsupported = errors.New("当前平台不支持该桌面能力")

// Notify 弹一条系统通知。
//
// 这是「关掉浏览器也能收到提醒」的落地：提醒到点时，
// 服务端既通过 WebSocket 推给在线页面，也弹一条系统通知。
// 两条通道互补 —— 前者面向开着页面的人，后者面向关了页面的人。
//
// 图标传空字符串：Windows 上会退化成应用默认图标。
// 要自定义图标需提供 .ico 绝对路径，但那要求图标随程序一起分发，
// 对本地自用的单人部署不划算。
func Notify(title, body string) error {
	return beeep.Notify(sanitizeToastText(title), sanitizeToastText(body), "")
}

// sanitizeToastText 去掉 Windows toast 不支持的字符。
//
// 踩坑记录（③ 开发时抓到）：go-toast 用 CDATA 拼 toast XML，
// **星位平面字符（超出 BMP 的 emoji，如 🤖 U+1F916）会让 XML 加载
// 直接失败**，报一个完全看不出原因的 doc.LoadXml 错误；
// 而 BMP 内的 emoji（⏰ U+23F0）和中文都完全正常。
// 通知是「关掉窗口也能收提醒」的关键通道，不能因为用户（或自己）的
// 标题里带了一个新 emoji 就整条消失，所以在这里统一兜底。
func sanitizeToastText(s string) string {
	if isASCII(s) {
		return s // 纯 ASCII 无需处理，绝大多数提醒标题走这条快路
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r > 0xFFFF {
			b.WriteString("·") // 星位平面字符（代理对）会破坏 toast XML
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
