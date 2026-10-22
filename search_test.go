package filesearch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxRejectsTraversal 是本项目最重要的安全测试：
// Agent 能读文件，就必须保证它读不到根目录之外的东西。
func TestSandboxRejectsTraversal(t *testing.T) {
	root := t.TempDir()

	// 只列在各平台语义下都确凿越界的输入。
	// 注意不要拿 "/etc/passwd" 当用例：Windows 下不带盘符的路径并非绝对路径，
	// 它会被 Join 到 root 之内，反而是安全行为（测试会误报）。
	// 同理 "../<root自身名字>/x" 经 Clean 后会解析回 root 内部，也不算越界。
	cases := []struct {
		name  string
		input string
	}{
		{"经典穿越", "../outside.txt"},
		{"深层穿越", "a/b/../../../../outside.txt"},
		{"纯上级目录", ".."},
		{"多级上级目录", "../../.."},
	}
	for _, c := range cases {
		if _, err := Sandbox(root, c.input); err == nil {
			t.Errorf("%s: Sandbox(%q) 应当被拒绝，实际通过了", c.name, c.input)
		}
	}

	// 合法路径必须放行
	ok, err := Sandbox(root, "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("合法子路径被拒绝: %v", err)
	}
	want := filepath.Join(root, "sub", "dir", "file.txt")
	if ok != want {
		t.Errorf("Sandbox = %q，期望 %q", ok, want)
	}

	// 根目录本身也必须放行
	if _, err := Sandbox(root, "."); err != nil {
		t.Errorf("根目录自身被拒绝: %v", err)
	}
}

func TestSearchFindsNameAndContent(t *testing.T) {
	root := t.TempDir()

	write(t, filepath.Join(root, "notes.md"), "今天讨论了 Eino 的流式转换\n还有一些别的内容\n")
	write(t, filepath.Join(root, "sub", "config.yaml"), "port: 8080\n")
	write(t, filepath.Join(root, "readme.txt"), "无关内容\n")

	// 按内容命中
	hits, err := Search(root, "Eino", 10)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("期望 1 条内容命中，实际 %d：%+v", len(hits), hits)
	}
	if hits[0].Path != "notes.md" {
		t.Errorf("命中路径 = %q，期望 notes.md", hits[0].Path)
	}
	if hits[0].Line != 1 {
		t.Errorf("命中行号 = %d，期望 1", hits[0].Line)
	}

	// 按文件名命中
	hits, _ = Search(root, "config.yaml", 10)
	if len(hits) == 0 || !strings.Contains(hits[0].Path, "config.yaml") {
		t.Errorf("文件名检索失败: %+v", hits)
	}
}

func TestSearchRespectsLimit(t *testing.T) {
	root := t.TempDir()
	// 造 50 个都含关键词的文件
	for i := 0; i < 50; i++ {
		write(t, filepath.Join(root, "f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".txt"), "hit me\n")
	}

	hits, _ := Search(root, "hit", 5)
	if len(hits) > 5 {
		t.Errorf("limit=5 却返回 %d 条 —— 不设上限会把模型上下文吃满", len(hits))
	}
}

func TestSearchSkipsIgnoredDirs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "secret.txt"), "hit me\n")
	write(t, filepath.Join(root, "node_modules", "pkg", "x.js"), "hit me\n")
	write(t, filepath.Join(root, "app.go"), "hit me\n")

	hits, _ := Search(root, "hit", 20)
	for _, h := range hits {
		if strings.Contains(h.Path, ".git") || strings.Contains(h.Path, "node_modules") {
			t.Errorf("应当跳过忽略目录，却命中 %q", h.Path)
		}
	}
	if len(hits) != 1 {
		t.Errorf("期望只剩 app.go 一条，实际 %d：%+v", len(hits), hits)
	}
}

func TestFormatEmpty(t *testing.T) {
	if got := Format(nil); !strings.Contains(got, "没有找到") {
		t.Errorf("空结果提示不正确: %q", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
