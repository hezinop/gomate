package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/tool"
)

// newTestEmitter 返回一个把事件记下来的 emitter。
func newTestEmitter() (*stepEmitter, *[]string) {
	var got []string
	em := newStepEmitter(func(event string, payload any) {
		b, _ := json.Marshal(payload)
		got = append(got, event+" "+string(b))
	})
	return em, &got
}

func TestStepHandlerToolLifecycle(t *testing.T) {
	em, got := newTestEmitter()
	h := StepHandler()
	ctx := withStepEmitter(context.Background(), em)

	// ChatModel 节点的回调必须被忽略：时间线只画工具调用
	modelInfo := &callbacks.RunInfo{Name: "ChatModel", Component: components.ComponentOfChatModel}
	h.OnStart(ctx, modelInfo, nil)
	h.OnEnd(ctx, modelInfo, nil)
	if len(*got) != 0 {
		t.Fatalf("非工具回调不应产生事件，得到 %v", *got)
	}

	// 工具开始 -> 结束（OnStart 返回的 ctx 携带开始时间，流到 OnEnd）
	toolInfo := &callbacks.RunInfo{Name: "add_memo", Component: components.ComponentOfTool}
	in := &tool.CallbackInput{ArgumentsInJSON: `{"content":"测试笔记"}`}
	ctx = h.OnStart(ctx, toolInfo, in)
	// 停几毫秒，让耗时可观测（ms=0 时会被 omitempty 省略）
	time.Sleep(5 * time.Millisecond)
	h.OnEnd(ctx, toolInfo, &tool.CallbackOutput{Response: "已保存 #12"})

	if len(*got) != 2 {
		t.Fatalf("应有 2 个事件，得到 %d 个: %v", len(*got), *got)
	}
	if !strings.HasPrefix((*got)[0], "step ") ||
		!strings.Contains((*got)[0], `"kind":"tool_start"`) ||
		!strings.Contains((*got)[0], `"name":"add_memo"`) ||
		!strings.Contains((*got)[0], "测试笔记") {
		t.Fatalf("tool_start 事件不对: %s", (*got)[0])
	}
	if !strings.Contains((*got)[1], `"kind":"tool_end"`) ||
		!strings.Contains((*got)[1], "已保存") ||
		!strings.Contains((*got)[1], `"ok":true`) ||
		!strings.Contains((*got)[1], `"ms"`) {
		t.Fatalf("tool_end 事件不对: %s", (*got)[1])
	}

	// 收集的步骤要能随消息落库
	js := em.json()
	if !strings.Contains(js, "tool_start") || !strings.Contains(js, "tool_end") {
		t.Fatalf("步骤 JSON 不对: %s", js)
	}
}

func TestStepHandlerToolError(t *testing.T) {
	em, got := newTestEmitter()
	h := StepHandler()
	ctx := withStepEmitter(context.Background(), em)

	toolInfo := &callbacks.RunInfo{Name: "list_todo", Component: components.ComponentOfTool}
	ctx = h.OnStart(ctx, toolInfo, &tool.CallbackInput{ArgumentsInJSON: "{}"})
	h.OnError(ctx, toolInfo, errors.New("MCP transport closed"))

	if len(*got) != 2 {
		t.Fatalf("应有 2 个事件，得到 %d 个", len(*got))
	}
	last := (*got)[1]
	if !strings.Contains(last, `"kind":"tool_end"`) ||
		!strings.Contains(last, `"ok":false`) ||
		!strings.Contains(last, "MCP transport closed") {
		t.Fatalf("失败的 tool_end 事件不对: %s", last)
	}
}

func TestStepHandlerSilentWithoutEmitter(t *testing.T) {
	// 命令行模式 ctx 里没有 emitter：必须静默且不 panic
	h := StepHandler()
	ctx := context.Background()
	toolInfo := &callbacks.RunInfo{Name: "add_memo", Component: components.ComponentOfTool}
	ctx = h.OnStart(ctx, toolInfo, &tool.CallbackInput{ArgumentsInJSON: "{}"})
	h.OnEnd(ctx, toolInfo, &tool.CallbackOutput{Response: "ok"})
	h.OnError(ctx, toolInfo, errors.New("x"))
	// 能走到这里不 panic 即通过
}

func TestStepEmitterEmptyJSON(t *testing.T) {
	em, _ := newTestEmitter()
	if js := em.json(); js != "" {
		t.Fatalf("无步骤时 JSON 应为空，得到 %q", js)
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	// 中文必须按整个字截断，不能切出乱码
	got := truncate("啊嗯哦哈哈嘿嘿", 3)
	if got != "啊嗯哦…" {
		t.Fatalf("截断结果不对: %q", got)
	}
	if got := truncate("短", 10); got != "短" {
		t.Fatalf("未超长不应截断: %q", got)
	}
}
