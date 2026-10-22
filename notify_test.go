package desktop

import "testing"

func TestSanitizeToastText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"纯 ASCII 原样返回", "hello world", "hello world"},
		{"中文与 BMP emoji 不受影响", "⏰ 提醒：喝水", "⏰ 提醒：喝水"},
		{"星位平面 emoji 被替换", "🤖 定时任务", "· 定时任务"},
		{"多个 emoji 各自替换", "🤖🤖", "··"},
		{"混合内容", "任务 🤖 完成", "任务 · 完成"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		if got := sanitizeToastText(c.in); got != c.want {
			t.Errorf("%s: sanitizeToastText(%q) = %q, 期望 %q", c.name, c.in, got, c.want)
		}
	}
}

func TestIsASCII(t *testing.T) {
	if !isASCII("plain ascii 123") {
		t.Error("纯 ASCII 应返回 true")
	}
	if isASCII("带中文") {
		t.Error("含非 ASCII 应返回 false")
	}
}
