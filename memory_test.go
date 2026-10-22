package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestEstimateTokens(t *testing.T) {
	// 中文按字计，英文按 4 字符 1 token —— 只需保证单调性与量级合理
	zh := EstimateTokens("你好世界")
	en := EstimateTokens("hello world hello world")
	if zh < 3 || zh > 8 {
		t.Errorf("中文 4 字估算 %d token，量级不合理", zh)
	}
	if en < 4 || en > 12 {
		t.Errorf("英文 23 字符估算 %d token，量级不合理", en)
	}
	if EstimateTokens("") != 0 {
		t.Error("空串应为 0")
	}
}

func TestTrimWithinBudgetKeepsAll(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("简短问题"),
		{Role: schema.Assistant, Content: "简短回答"},
	}
	kept, evicted := Trim(msgs, 10000)
	if len(kept) != 2 || len(evicted) != 0 {
		t.Errorf("未超预算却被裁剪: kept=%d evicted=%d", len(kept), len(evicted))
	}
}

// TestTrimDropsOldest 验证裁剪是从头部（最旧）开始的。
func TestTrimDropsOldest(t *testing.T) {
	var msgs []*schema.Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, schema.UserMessage("第"+strings.Repeat("很", 200)+"长的一轮 "+string(rune('a'+i))))
		msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "回答" + string(rune('a'+i))})
	}

	kept, evicted := Trim(msgs, 500)
	if len(evicted) == 0 {
		t.Fatal("超预算却没有挤出任何历史")
	}
	if len(kept)+len(evicted) != len(msgs) {
		t.Errorf("kept+evicted = %d，应当等于总数 %d", len(kept)+len(evicted), len(msgs))
	}
	// 保留的必须是最新的一批
	if !strings.Contains(kept[len(kept)-1].Content, string(rune('a'+9))) {
		t.Errorf("最新一轮被裁掉了，kept 末条 = %q", kept[len(kept)-1].Content)
	}
	// 被挤出的必须是最旧的一批
	if !strings.Contains(evicted[0].Content, string(rune('a'+0))) {
		t.Errorf("最旧一轮不在 evicted 里，evicted 首条 = %q", evicted[0].Content)
	}
}

// TestTrimKeepsQAPairs 验证不会把「用户提问」和「助手回答」切开 ——
// 否则模型会看到"问了但没答"的残缺上下文。
func TestTrimKeepsQAPairs(t *testing.T) {
	var msgs []*schema.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, schema.UserMessage(strings.Repeat("问", 300)+string(rune('a'+i))))
		msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "答" + string(rune('a'+i))})
	}

	kept, _ := Trim(msgs, 400)
	if len(kept) == 0 {
		t.Fatal("全部被裁掉了")
	}
	if kept[0].Role != schema.User {
		t.Errorf("裁剪后首条应为 user（保持问答对完整），实际 role=%v", kept[0].Role)
	}
}

func TestSummaryStoreRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatalf("打开摘要库失败: %v", err)
	}
	defer s.Close()

	if got, err := s.Get("s1"); err != nil || got != "" {
		t.Fatalf("空库应返回空串，实际 %q, err=%v", got, err)
	}
	if err := s.Save("s1", "这是摘要"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, _ := s.Get("s1")
	if got != "这是摘要" {
		t.Errorf("读回 %q，期望「这是摘要」", got)
	}
	// 覆盖
	if err := s.Save("s1", "更新后的摘要"); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if got, _ = s.Get("s1"); got != "更新后的摘要" {
		t.Errorf("覆盖后读回 %q", got)
	}
}

// TestPrepareInjectsSummary 验证长期摘要会被塞进送给模型的消息里。
func TestPrepareInjectsSummary(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatalf("打开摘要库失败: %v", err)
	}
	defer s.Close()
	if err := s.Save("sess", "用户之前决定用 Eino"); err != nil {
		t.Fatal(err)
	}

	var called int
	m := New(s, 10000, func(ctx context.Context, evicted []*schema.Message, prev string) (string, error) {
		called++
		return "", nil
	})

	out, err := m.Prepare(context.Background(), "sess", []*schema.Message{schema.UserMessage("你好")})
	if err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("期望 2 条（摘要 + 问题），实际 %d", len(out))
	}
	if out[0].Role != schema.System {
		t.Errorf("首条应为 system 摘要，实际 role=%v", out[0].Role)
	}
	if !strings.Contains(out[0].Content, "Eino") {
		t.Errorf("摘要未注入: %q", out[0].Content)
	}
	if called != 0 {
		t.Errorf("未挤出历史却触发了压缩，调用了 %d 次", called)
	}
}

// TestPrepareSummarizesWhenEvicted 验证被挤出窗口时会触发压缩。
func TestPrepareSummarizesWhenEvicted(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatalf("打开摘要库失败: %v", err)
	}
	defer s.Close()

	var gotPrev string
	var gotEvicted int
	m := New(s, 200, func(ctx context.Context, evicted []*schema.Message, prev string) (string, error) {
		gotEvicted = len(evicted)
		gotPrev = prev
		return "压缩结果", nil
	})

	var history []*schema.Message
	for i := 0; i < 8; i++ {
		history = append(history, schema.UserMessage(strings.Repeat("内容", 100)))
		history = append(history, &schema.Message{Role: schema.Assistant, Content: "回答"})
	}

	if _, err := m.Prepare(context.Background(), "sess2", history); err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	if gotEvicted == 0 {
		t.Error("历史被挤出却没触发压缩")
	}
	_ = gotPrev
	if saved, _ := s.Get("sess2"); saved != "压缩结果" {
		t.Errorf("摘要未持久化，读到 %q", saved)
	}
}

// TestPrepareSurvivesSummarizeFailure 验证压缩失败不会阻断对话。
func TestPrepareSurvivesSummarizeFailure(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	m := New(s, 200, func(ctx context.Context, evicted []*schema.Message, prev string) (string, error) {
		return "", context.DeadlineExceeded
	})

	var history []*schema.Message
	for i := 0; i < 8; i++ {
		history = append(history, schema.UserMessage(strings.Repeat("内容", 100)))
		history = append(history, &schema.Message{Role: schema.Assistant, Content: "回答"})
	}

	out, err := m.Prepare(context.Background(), "sess3", history)
	if err != nil {
		t.Fatalf("压缩失败不应阻断对话，却返回了错误: %v", err)
	}
	if len(out) == 0 {
		t.Error("压缩失败后仍应返回可用的历史")
	}
}
