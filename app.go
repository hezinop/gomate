// Package app 收敛「把各模块装配成一个可运行实例」这段逻辑。
//
// 为什么单独抽出来：Web 服务（cmd/gomate serve）和桌面版（cmd/gomated）
// 需要完全相同的装配 —— 同样的 agent、记忆、提醒库、推送中心、埋点。
// 复制两份的话，加一个模块要改两个地方，迟早不一致。
package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	openai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"

	"github.com/hizenop/gomate/internal/agent"
	"github.com/hizenop/gomate/internal/chatsession"
	"github.com/hizenop/gomate/internal/config"
	"github.com/hizenop/gomate/internal/memory"
	"github.com/hizenop/gomate/internal/push"
	"github.com/hizenop/gomate/internal/reminder"
	"github.com/hizenop/gomate/internal/server"
	"github.com/hizenop/gomate/internal/telemetry"
)

// Runtime 是一次装配产出的全部运行时依赖。
type Runtime struct {
	Cfg          *config.Config
	Assistant    *agent.Assistant
	Memory       *memory.Manager
	Hub          *push.Hub
	Reminders    *reminder.Store
	Summary      *memory.Store
	ChatSessions *chatsession.Store
}

// New 装配一个完整的运行时。返回的 Runtime 由调用方负责 Close。
func New(ctx context.Context, cfg *config.Config) (*Runtime, error) {
	as, err := agent.New(ctx, cfg, MCPEnv)
	if err != nil {
		return nil, err
	}

	summaryStore, err := memory.Open(cfg.SummaryDBPath)
	if err != nil {
		as.Close()
		return nil, fmt.Errorf("打开摘要库失败: %w", err)
	}

	remindStore, err := reminder.Open(cfg.ReminderDBPath)
	if err != nil {
		as.Close()
		summaryStore.Close()
		return nil, fmt.Errorf("打开提醒库失败: %w", err)
	}

	// 长期记忆的压缩器：单独用一个不带工具的 ChatModel，
	// 避免与主链路的工具/schema 相互干扰。
	summarizer, err := newSummarizer(ctx, cfg)
	if err != nil {
		as.Close()
		summaryStore.Close()
		remindStore.Close()
		return nil, err
	}

	chatStore, err := chatsession.Open(cfg.ChatDBPath)
	if err != nil {
		as.Close()
		summaryStore.Close()
		remindStore.Close()
		return nil, fmt.Errorf("打开会话库失败: %w", err)
	}

	// 执行过程可视化（②）：把工具调用步骤转成 SSE 事件的回调。
	// 全局注册一次；没有 emitter 的 ctx（命令行模式）下自动静默。
	callbacks.AppendGlobalHandlers(server.StepHandler())

	return &Runtime{
		Cfg:          cfg,
		Assistant:    as,
		Memory:       memory.New(summaryStore, cfg.MemoryBudget, summarizer),
		Hub:          push.New(128),
		Reminders:    remindStore,
		Summary:      summaryStore,
		ChatSessions: chatStore,
	}, nil
}

func (r *Runtime) Close() {
	r.Assistant.Close()
	r.Summary.Close()
	r.Reminders.Close()
	r.ChatSessions.Close()
}

// Server 构造接入层。
//
// notify 可为 nil —— 命令行与纯 Web 模式不弹系统通知。
// tracer 可为 nil —— 埋点是可选的。
func (r *Runtime) Server(tracer *telemetry.Tracer, notify func(string, string) error) *server.Server {
	return server.New(server.Deps{
		Cfg:          r.Cfg,
		Assistant:    r.Assistant,
		Memory:       r.Memory,
		Tracer:       tracer,
		Hub:          r.Hub,
		Reminders:    r.Reminders,
		ChatSessions: r.ChatSessions,
		Notify:       notify,
	})
}

// MCPEnv 生成 MCP 子进程的完整环境变量。
//
// 传入的 kv（如 GOMATE_MEMO_DB=...）追加在 os.Environ() 之后，
// 靠后的同名变量覆盖靠前的，从而把「本进程解析出的绝对路径」可靠地传给子进程。
// 之所以要显式传而不是让子进程自己读 .env：.env 只被本进程的 config 层解析，
// 从没进过进程环境变量，子进程是看不到的。
//
// 导出是因为命令行模式（cmd/gomate）也要用它来构造 agent。
func MCPEnv(kv ...string) []string {
	return append(os.Environ(), kv...)
}

// newSummarizer 构造「把历史压缩成摘要」的函数。
//
// 这是一次额外的 LLM 调用，只在历史被挤出窗口时才触发（见 memory.Prepare），
// 不会每轮都烧钱。
func newSummarizer(ctx context.Context, cfg *config.Config) (memory.Summarizer, error) {
	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.LLMAPIKey,
		BaseURL: cfg.LLMBaseURL,
		Model:   cfg.LLMModel,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("构造摘要模型失败: %w", err)
	}

	return func(ctx context.Context, evicted []*schema.Message, prev string) (string, error) {
		var b strings.Builder
		b.WriteString("请把下面这段对话压缩成一份简洁的中文摘要，保留事实、决定、待办与时间信息，")
		b.WriteString("便于后续对话延续上下文。只输出摘要本身，不要解释。\n\n")
		if prev != "" {
			b.WriteString("【已有摘要】\n" + prev + "\n\n")
		}
		b.WriteString("【新增对话】\n")
		for _, m := range evicted {
			role := "用户"
			if m.Role == schema.Assistant {
				role = "助手"
			}
			b.WriteString(role + "：" + m.Content + "\n")
		}
		msg, err := cm.Generate(ctx, []*schema.Message{schema.UserMessage(b.String())})
		if err != nil {
			return "", err
		}
		return msg.Content, nil
	}, nil
}
