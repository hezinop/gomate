package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeDotEnv 在 dir 下写一个 .env 文件。
func writeDotEnv(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, dotEnvName), []byte(body), 0o600); err != nil {
		t.Fatalf("写 .env 失败: %v", err)
	}
}

// chdir 切换工作目录，并在测试结束后恢复。
// 注意：这会成为进程级副作用，所以本文件的用例都不能用 t.Parallel()。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换目录失败: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("恢复目录失败: %v", err)
		}
	})
}

// clearEnvKeys 把可能污染测试的环境变量清空。
// 用例需要 .env 文件生效时，必须先确保进程环境变量是空的。
func clearEnvKeys(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LLM_API_KEY", "LLM_BASE_URL", "LLM_MODEL", "GOMATE_MEMO_DB", "GOMATE_MEMO_BIN"} {
		t.Setenv(k, "")
	}
}

func TestLoadReadsDotEnvFile(t *testing.T) {
	clearEnvKeys(t)
	dir := t.TempDir()
	writeDotEnv(t, dir, "LLM_API_KEY=sk-from-file\nLLM_MODEL=qwen-plus\n")
	chdir(t, dir)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c.LLMAPIKey != "sk-from-file" {
		t.Errorf("APIKey = %q，期望来自 .env 的 sk-from-file", c.LLMAPIKey)
	}
	if c.LLMModel != "qwen-plus" {
		t.Errorf("Model = %q，期望 qwen-plus", c.LLMModel)
	}
	if c.sourceOf("LLM_API_KEY") != SourceDotEnv {
		t.Errorf("来源 = %v，期望 SourceDotEnv", c.sourceOf("LLM_API_KEY"))
	}
}

func TestProcessEnvOverridesDotEnv(t *testing.T) {
	clearEnvKeys(t)
	dir := t.TempDir()
	writeDotEnv(t, dir, "LLM_API_KEY=sk-from-file\n")
	// 显式设置环境变量，应当覆盖 .env —— 这是生产容器用真环境变量顶掉文件的依据
	t.Setenv("LLM_API_KEY", "sk-from-env")
	chdir(t, dir)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c.LLMAPIKey != "sk-from-env" {
		t.Errorf("APIKey = %q，期望环境变量优先的 sk-from-env", c.LLMAPIKey)
	}
	if c.sourceOf("LLM_API_KEY") != SourceEnv {
		t.Errorf("来源 = %v，期望 SourceEnv", c.sourceOf("LLM_API_KEY"))
	}
}

func TestUpwardSearchFindsProjectRootDotEnv(t *testing.T) {
	clearEnvKeys(t)
	root := t.TempDir()
	writeDotEnv(t, root, "LLM_API_KEY=sk-parent\n")

	// 从 bin/ 子目录启动，应当一路向上找到项目根的 .env
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("创建子目录失败: %v", err)
	}
	chdir(t, binDir)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c.LLMAPIKey != "sk-parent" {
		t.Errorf("APIKey = %q，期望来自上层目录的 sk-parent", c.LLMAPIKey)
	}
	if c.ProjectRoot != root {
		t.Errorf("ProjectRoot = %q，期望 %q", c.ProjectRoot, root)
	}
}

func TestMissingKeyProducesDiagnostic(t *testing.T) {
	clearEnvKeys(t)
	dir := t.TempDir()
	// .env 存在但缺 LLM_API_KEY —— 正是用户实际踩到的场景
	writeDotEnv(t, dir, "LLM_BASE_URL=https://api.deepseek.com\nLLM_MODEL=deepseek-chat\n")
	chdir(t, dir)

	_, err := Load()
	if err == nil {
		t.Fatal("期望报错，实际返回了 nil")
	}
	msg := err.Error()
	for _, want := range []string{"LLM_API_KEY", ".env", "修复方式"} {
		if !strings.Contains(msg, want) {
			t.Errorf("诊断信息不完整，缺少 %q：\n%s", want, msg)
		}
	}
	// 诊断里应当列出文件里实际存在的键，帮用户一眼看出漏了哪个
	if !strings.Contains(msg, "LLM_BASE_URL") {
		t.Errorf("诊断信息应当提示文件中已有的键：\n%s", msg)
	}
}

func TestRelativePathsResolvedAgainstProjectRoot(t *testing.T) {
	clearEnvKeys(t)
	root := t.TempDir()
	writeDotEnv(t, root, "LLM_API_KEY=sk-x\n")
	chdir(t, root)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	wantDB := filepath.Join(root, "var", "memo.db")
	if c.MemoDBPath != wantDB {
		t.Errorf("MemoDBPath = %q，期望绝对路径 %q", c.MemoDBPath, wantDB)
	}
	if !filepath.IsAbs(c.MemoDBPath) {
		t.Errorf("MemoDBPath 必须是绝对路径，实际 %q", c.MemoDBPath)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(c.MemoBinPath, ".exe") {
		t.Errorf("Windows 下 MemoBinPath 默认应带 .exe，实际 %q", c.MemoBinPath)
	}
}

func TestMask(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "(空)"},
		{"abc", "***"},
		{"sk-1234567890abcdef", "sk-1" + strings.Repeat("*", 8) + "cdef"},
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.want {
			t.Errorf("Mask(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 脱敏后绝不允许出现原始密钥片段以外的完整内容
	full := "sk-secretkey-1234567890"
	masked := Mask(full)
	if strings.Contains(masked, "secretkey") {
		t.Errorf("脱敏失败，泄漏了原始内容: %s", masked)
	}
}
