package server

// 执行过程可视化（②）的实时通道。
//
// 要解决的核心问题：react agent 的中间轮工具调用流被图分支的检查器消费掉了，
// 调用方（handleChat）从 StreamReader 里只能看到最终答案，看不到工具调用。
// 但 eino 的回调系统会在【每个工具执行】时带上工具自己的 RunInfo 触发
// OnStart/OnEnd（见 compose/tool_node.go 的 ReuseHandlers），
// 因此把工具执行步骤转成 SSE 事件的正确位置是回调 handler，
// 而不是去解构流式 chunk。
//
// 数据流：
//
//	工具执行 -> callbacks.OnStart/OnEnd -> stepEmitter.emit -> SSE "step" 事件 -> 前端时间线
//	                                                  \-> 收集进 steps，随消息落库（① 的 tool_calls 列）

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/tool"
)

// step 是 Agent 执行过程中的一步（一次工具调用）。
type step struct {
	Kind   string `json:"kind"` // tool_start / tool_end
	Name   string `json:"name"` // 工具名，如 add_memo
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	Ms     int64  `json:"ms,omitempty"` // 耗时毫秒（tool_end 才有）
	OK     bool   `json:"ok"`
}

// stepEmitter 把工具步骤实时推给前端，并收集起来供落库。
//
// 并发说明：eino 的 tools node 可能并行执行多个工具，回调会在不同goroutine
// 触发；而 SSE 写入不是并发安全的。用一把 mu 同时保护 steps 切片和
// emit 写入 —— emit 只是写内存缓冲 + flush，持锁时间可忽略。
type stepEmitter struct {
	mu    sync.Mutex
	emit  func(event string, payload any)
	steps []step
}

func newStepEmitter(emit func(event string, payload any)) *stepEmitter {
	return &stepEmitter{emit: emit}
}

// start 处理工具开始执行：推送 tool_start，并把开始时间藏进 ctx 供 end 取用。
// （与 telemetry 用 ctx 传 span id 是同一个技巧：OnStart 返回的 ctx 会流到 OnEnd。）
func (e *stepEmitter) start(ctx context.Context, name, args string) context.Context {
	e.emitStep(step{Kind: "tool_start", Name: name, Args: args})
	return context.WithValue(ctx, stepStartKey{}, time.Now())
}

func (e *stepEmitter) end(ctx context.Context, name, result string, ok bool) {
	var ms int64
	if t0, has := ctx.Value(stepStartKey{}).(time.Time); has {
		ms = time.Since(t0).Milliseconds()
	}
	e.emitStep(step{Kind: "tool_end", Name: name, Result: result, Ms: ms, OK: ok})
}

func (e *stepEmitter) emitStep(s step) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.steps = append(e.steps, s)
	e.emit("step", s)
}

// json 返回收集到的步骤 JSON，供随 assistant 消息落库。
func (e *stepEmitter) json() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.steps) == 0 {
		return ""
	}
	b, err := json.Marshal(e.steps)
	if err != nil {
		return ""
	}
	return string(b)
}

// stepStartKey 是存在 ctx 里的工具开始时间。
type stepStartKey struct{}

// emitterKey 是存在 ctx 里的 stepEmitter。
type emitterKey struct{}

func withStepEmitter(ctx context.Context, e *stepEmitter) context.Context {
	return context.WithValue(ctx, emitterKey{}, e)
}

func stepEmitterFrom(ctx context.Context) *stepEmitter {
	e, _ := ctx.Value(emitterKey{}).(*stepEmitter)
	return e
}

// StepHandler 构造「把工具执行步骤转成 SSE 事件」的回调 handler。
//
// 在 app.New 里全局注册。ctx 里没有 emitter 时（命令行 --once 模式）
// 自动静默，不产生任何副作用。
func StepHandler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, in callbacks.CallbackInput) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx // 只关心单个工具的执行，不关心 ChatModel/ToolsNode 节点
			}
			e := stepEmitterFrom(ctx)
			if e == nil {
				return ctx
			}
			var args string
			if ti := tool.ConvCallbackInput(in); ti != nil {
				args = truncate(ti.ArgumentsInJSON, 120)
			}
			return e.start(ctx, info.Name, args)
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, out callbacks.CallbackOutput) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx
			}
			e := stepEmitterFrom(ctx)
			if e == nil {
				return ctx
			}
			var result string
			if to := tool.ConvCallbackOutput(out); to != nil {
				result = truncate(to.Response, 160)
			}
			e.end(ctx, info.Name, result, true)
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx
			}
			e := stepEmitterFrom(ctx)
			if e == nil {
				return ctx
			}
			msg := "未知错误"
			if err != nil {
				msg = truncate(err.Error(), 160)
			}
			e.end(ctx, info.Name, msg, false)
			return ctx
		}).
		Build()
}

// truncate 按 rune 截断，避免把中文切坏。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
