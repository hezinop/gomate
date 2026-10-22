// Package telemetry 提供 Agent 执行链路的 span 记录与 token 计量。
//
// 为什么自己写而不是直接上 OpenTelemetry SDK：
//
//  1. Eino 官方生态没有现成的 OTel callbacks adapter（已核实不存在），
//     而 Eino 对外暴露的标准接入点就是 callbacks.Handler —— 本项目实现它，
//     将来要接真 OTel 只需把 Exporter 换掉，业务代码零改动。
//  2. Agent 最难查的 bug 是「它为什么走了这条路」。本项目先把
//     「提问 → 思考 → 调工具 → 再思考 → 回答」的每一步记成 span，
//     在 UI 里直接看得到，排障价值立刻兑现，不必先搭 collector。
//
// 设计要点：只注册 OnStart / OnEnd / OnError 三个非流式时机。
// 流式回调会收到一份 stream 拷贝且**必须**由 handler 负责关闭，
// 漏关会导致整条流水线 goroutine 与内存泄漏 —— 不注册就没有这个负担。
// 流式调用的 token 用量改从最终消息的 ResponseMeta.Usage 读取。
package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
)

// Span 是一次组件调用。
type Span struct {
	ID       int64         `json:"id"`
	Name     string        `json:"name"` // 图节点名，如 ChatModel / ToolsNode
	Type     string        `json:"type"` // 实现标识，如 OpenAI
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"` // Go 1.24 起 time.Duration 可直接被 encoding/json 序列化
	Detail   string        `json:"detail"`   // 人类可读的输入摘要

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	Error string `json:"error,omitempty"`
}

// Trace 是一次会话（一次用户提问）的完整链路。
type Trace struct {
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`

	mu    sync.Mutex
	spans []Span
	next  int64

	// open 记录已开始但尚未结束的 span，供 Finish 时兜底关闭。
	// 流式调用不会触发 OnEnd，靠这里补上耗时。
	open map[int64]time.Time
}

// Finish 关闭所有仍未结束的 span，并计算累计 token。
func (t *Trace) Finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, start := range t.open {
		for i := range t.spans {
			if t.spans[i].ID == id {
				t.spans[i].Duration = now.Sub(start)
			}
		}
	}
	t.open = nil
}

// Spans 返回 span 快照（副本，避免调用方拿到内部切片产生数据竞争）。
func (t *Trace) Spans() []Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Span, len(t.spans))
	copy(out, t.spans)
	return out
}

// Totals 汇总本次会话的 token 消耗与总耗时。
func (t *Trace) Totals() (prompt, completion, total int, elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.spans {
		prompt += s.PromptTokens
		completion += s.CompletionTokens
		total += s.TotalTokens
	}
	elapsed = time.Since(t.StartedAt)
	return
}

// AsText 渲染成缩进树，方便在日志与前端直接看。
func (t *Trace) AsText() string {
	var b strings.Builder
	_, _, total, elapsed := t.Totals()
	fmt.Fprintf(&b, "会话 %s  总耗时 %v  总 token %d\n", t.SessionID, elapsed.Round(time.Millisecond), total)
	for _, s := range t.Spans() {
		dur := s.Duration
		if dur == 0 {
			dur = time.Since(s.Start)
		}
		fmt.Fprintf(&b, "  [%7v] %-14s %s", dur.Round(time.Millisecond), label(s), s.Detail)
		if s.TotalTokens > 0 {
			fmt.Fprintf(&b, "  (token %d)", s.TotalTokens)
		}
		if s.Error != "" {
			fmt.Fprintf(&b, "  ERROR: %s", s.Error)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func label(s Span) string {
	if s.Name != "" {
		return s.Name
	}
	if s.Type != "" {
		return s.Type
	}
	return "component"
}

// ---------------------------------------------------------------- 上下文

type traceKey struct{}
type spanKey struct{}

// WithTrace 把 trace 挂进 context，并返回一个可结束它的函数。
func WithTrace(ctx context.Context, t *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom 从 context 取出 trace；不存在返回 nil。
func TraceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// ---------------------------------------------------------------- Tracer

// Tracer 持有 callback handler，并保留最近的若干条 trace 供查询。
type Tracer struct {
	mu     sync.Mutex
	traces map[string]*Trace
	order  []string // 用于淘汰最旧的 trace
	limit  int
}

// New 创建 Tracer。limit 控制最多保留多少条会话 trace（防止内存无限增长）。
func New(limit int) *Tracer {
	if limit <= 0 {
		limit = 50
	}
	return &Tracer{traces: make(map[string]*Trace), limit: limit}
}

// Start 开始一条新会话 trace，返回带 trace 的 ctx。
func (tr *Tracer) Start(ctx context.Context, sessionID string) (context.Context, *Trace) {
	t := &Trace{
		SessionID: sessionID,
		StartedAt: time.Now(),
		open:      make(map[int64]time.Time),
	}

	tr.mu.Lock()
	tr.traces[sessionID] = t
	tr.order = append(tr.order, sessionID)
	for len(tr.order) > tr.limit {
		oldest := tr.order[0]
		tr.order = tr.order[1:]
		delete(tr.traces, oldest)
	}
	tr.mu.Unlock()

	return WithTrace(ctx, t), t
}

// Get 取一条历史 trace。
func (tr *Tracer) Get(sessionID string) *Trace {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.traces[sessionID]
}

// Handler 构造 Eino 的 callback handler。
// 只注册非流式三个时机，理由见包注释。
func (tr *Tracer) Handler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			t := TraceFrom(ctx)
			if t == nil {
				return ctx
			}
			var name, typ string
			if info != nil {
				name, typ = info.Name, info.Type
			}

			t.mu.Lock()
			t.next++
			s := Span{ID: t.next, Name: name, Type: typ, Start: time.Now(), Detail: summarize(input)}
			t.spans = append(t.spans, s)
			t.open[s.ID] = s.Start
			t.mu.Unlock()

			return context.WithValue(ctx, spanKey{}, s.ID)
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			t := TraceFrom(ctx)
			if t == nil {
				return ctx
			}
			id, _ := ctx.Value(spanKey{}).(int64)
			t.mu.Lock()
			defer t.mu.Unlock()

			for i := range t.spans {
				if t.spans[i].ID != id {
					continue
				}
				t.spans[i].Duration = time.Since(t.spans[i].Start)
				delete(t.open, id)
				// ChatModel 的输出里带 token 用量，这是成本控制的唯一可靠来源
				if out := model.ConvCallbackOutput(output); out != nil && out.TokenUsage != nil {
					t.spans[i].PromptTokens = out.TokenUsage.PromptTokens
					t.spans[i].CompletionTokens = out.TokenUsage.CompletionTokens
					t.spans[i].TotalTokens = out.TokenUsage.TotalTokens
				}
			}
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			t := TraceFrom(ctx)
			if t == nil {
				return ctx
			}
			id, _ := ctx.Value(spanKey{}).(int64)
			t.mu.Lock()
			defer t.mu.Unlock()
			for i := range t.spans {
				if t.spans[i].ID != id {
					continue
				}
				t.spans[i].Duration = time.Since(t.spans[i].Start)
				t.spans[i].Error = err.Error()
				delete(t.open, id)
			}
			return ctx
		}).
		Build()
}

// AddUsage 手动补记 token 用量。
// 流式调用不会触发 OnEnd（我们没注册流��回调），
// 因此从最终消息的 ResponseMeta.Usage 里拿到用量后，需要回填到最后一条 span。
func (tr *Tracer) AddUsage(ctx context.Context, prompt, completion int) {
	t := TraceFrom(ctx)
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.spans) - 1; i >= 0; i-- {
		if t.spans[i].TotalTokens == 0 {
			t.spans[i].PromptTokens = prompt
			t.spans[i].CompletionTokens = completion
			t.spans[i].TotalTokens = prompt + completion
			return
		}
	}
}

// summarize 生成输入的简短摘要。刻意做截断 ——
// 完整 prompt 可能上万字符，全量记录会让 trace 自己变成内存负担。
func summarize(input callbacks.CallbackInput) string {
	if in := model.ConvCallbackInput(input); in != nil {
		last := ""
		if n := len(in.Messages); n > 0 {
			last = in.Messages[n-1].Content
		}
		return fmt.Sprintf("LLM 调用，%d 条消息，末条：%q", len(in.Messages), truncate(last, 60))
	}
	return ""
}

func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
