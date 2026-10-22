package server

// 定时 Agent 任务（③ 主动式 Agent）的执行逻辑。
//
// 与普通提醒的差别：到点后不是弹一条通知，而是用 Assistant.Generate
// 跑一轮完整的 ReAct（带全部 MCP 工具），把产出结果推送给人。
// 典型场景：「明早 9 点总结我的待办和昨天的笔记，生成一份晨报」。
//
// 三个关键设计：
//  1. **异步执行**：调度循环每秒 tick，onFire 里跑几十秒的 Agent 会把
//     后续到点任务全堵迟。ClaimDue 已把任务标记为 fired（防重），
//     这里只负责执行、回写、推送。
//  2. **信号量限流**：到点任务可能扎堆（多个 9:00），不限流会同时
//     打爆 LLM 配额与 MCP 子进程。
//  3. **注入当前时间**：没有它，「总结今天的待办」里的「今天」
//     对模型来说是未知的。

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/hizenop/gomate/internal/push"
	"github.com/hizenop/gomate/internal/reminder"
)

// agentTaskSystemPrompt 是任务执行的系统提示。
const agentTaskSystemPrompt = `你是 GoMate 的定时任务执行器。现在是任务到点时间，请直接完成下面指定的任务。
你可以使用全部工具（待办、笔记、文件检索）收集信息。
要求：
1. 直接给出任务结果，不要向用户提问或要求确认
2. 结果简洁、完整，适合直接推送给用户阅读
3. 使用中文`

// agentTaskSem 限制同时执行的任务数。
var agentTaskSem = make(chan struct{}, 3)

// runAgentTask 异步执行一个到点的 agent 任务。
// parent 只用于「排队等待期间进程退出」的中断，执行本身用独立 ctx，
// 避免服务端正常关闭时把已领取的任务全部判失败。
func (s *Server) runAgentTask(parent context.Context, r reminder.Reminder) {
	select {
	case agentTaskSem <- struct{}{}:
	case <-parent.Done():
		return
	}

	go func() {
		defer func() { <-agentTaskSem }()

		ctx, cancel := context.WithTimeout(context.Background(), s.deps.Cfg.AgentTaskTimeout)
		defer cancel()

		fmt.Fprintf(os.Stderr, "server: agent 任务开始执行 id=%d %q\n", r.ID, r.Title)

		msg, err := s.deps.Assistant.Generate(ctx, []*schema.Message{
			schema.SystemMessage(agentTaskSystemPrompt),
			schema.UserMessage(fmt.Sprintf("【当前时间】%s\n\n【任务】%s",
				time.Now().Format("2006-01-02 15:04:05"), r.Prompt)),
		})

		var (
			text string
			ok   bool
		)
		switch {
		case err != nil:
			text = "任务执行失败：" + err.Error()
		case msg == nil:
			text = "任务执行失败：模型没有返回内容"
		default:
			text, ok = msg.Content, true
		}

		// 回写结果（失败也写：用户在列表里能看到发生了什么，而不是石沉大海）
		if s.deps.Reminders != nil {
			if err := s.deps.Reminders.FinishAgentTask(r.ID, text); err != nil {
				fmt.Fprintf(os.Stderr, "server: agent 任务结果回写失败 id=%d: %v\n", r.ID, err)
			}
		}

		// 通道一：在线页面
		s.deps.Hub.Broadcast(push.TypeAgentTask, map[string]any{
			"id":     r.ID,
			"title":  r.Title,
			"result": text,
			"ok":     ok,
		})

		// 通道二：系统通知（关掉窗口也能收到，这是桌面版的核心价值）
		// 标题刻意不放 emoji：go-toast 的 toast XML 会被星位平面字符炸掉
		// （见 desktop.sanitizeToastText 的踩坑记录），统一走纯文本前缀。
		if s.deps.Notify != nil {
			title := "定时任务：" + r.Title
			if !ok {
				title += "（失败）"
			}
			if err := s.deps.Notify(title, notifyBody(text)); err != nil {
				fmt.Fprintf(os.Stderr, "server: agent 任务通知失败 id=%d: %v\n", r.ID, err)
			}
		}

		fmt.Fprintf(os.Stderr, "server: agent 任务完成 id=%d ok=%v 结果长度=%d\n",
			r.ID, ok, len([]rune(text)))
	}()
}

// notifyBody 把任务结果裁剪成适合系统通知的长度。
// Windows 通知正文过长会被系统截断，还会顶爆 toast 模板。
func notifyBody(text string) string {
	r := []rune(text)
	if len(r) <= 120 {
		return text
	}
	return string(r[:120]) + "…"
}
