// Package server 是 GoMate 的接入层：SSE 流式响应 + WebSocket 主动推送 + 静态页面。
//
// 两条通道各司其职，不要混用：
//   - **SSE（POST /api/chat）**：请求-响应式的流式输出，用来做打字机效果。
//     为什么不用 WebSocket 做这个：SSE 天然是 HTTP，穿透性好、断线自动重连，
//     而一问一答本来也不需要客户端反向发帧。
//   - **WebSocket（GET /api/ws）**：服务端主动推送，用来送提醒到点、
//     后台任务完成这类「不是用户问出来的」消息。
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/hizenop/gomate/internal/agent"
	"github.com/hizenop/gomate/internal/chatsession"
	"github.com/hizenop/gomate/internal/config"
	"github.com/hizenop/gomate/internal/memory"
	"github.com/hizenop/gomate/internal/push"
	"github.com/hizenop/gomate/internal/reminder"
	"github.com/hizenop/gomate/internal/telemetry"
)

//go:embed web/index.html
var indexHTML []byte

// maxHistoryTurns 单个会话最多保留多少轮（一问一答算一轮）。
// 与 memory 的 token 预算是两道独立的防线：
// 前者防内存无限增长，后者防模型上下文撑爆。
const maxHistoryTurns = 20

// Deps 是 Server 的全部依赖，显式传入以便测试时替换。
type Deps struct {
	Cfg          *config.Config
	Assistant    *agent.Assistant
	Memory       *memory.Manager
	Tracer       *telemetry.Tracer
	Hub          *push.Hub
	Reminders    *reminder.Store
	ChatSessions *chatsession.Store

	// Notify 可选。桌面端注入它后，提醒到点会额外弹一条系统通知。
	//
	// 为什么需要第二条通道：WebSocket 只推给「开着页面的人」，
	// 浏览器一关就什么都没有了，而提醒恰恰常常发生在页面早已关掉之后。
	// 为 nil 时（命令行 / 纯 Web 模式）行为与以前完全一致，不引入副作用。
	//
	// 这里刻意不 import desktop 包：接入层不该依赖平台能力，
	// 传入函数而不是具体实现，也方便测试时替换成一个记录器。
	Notify func(title, body string) error
}

// Server 持有接入层的全部状态。
type Server struct {
	deps Deps

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	history []*schema.Message
	tokens  int // 本会话累计消耗的 token，用于成本控制
}

// New 创建 Server。
func New(d Deps) *Server {
	return &Server{
		deps:     d,
		sessions: make(map[string]*session),
	}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/ws", s.deps.Hub.ServeWS)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/trace", s.handleTrace)
	mux.HandleFunc("/api/health", s.handleHealth)

	// 会话管理（① 会话持久化）。用 Go 1.22 的 method+pattern 语法，
	// GET/DELETE 在同一路径 pattern 上不会互相冲突。
	mux.HandleFunc("GET /api/sessions", s.handleSessionList)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.handleSessionMessages)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	return mux
}

// Run 启动 HTTP 服务与提醒调度循环，阻塞直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	// 提醒调度：到点后通过 WebSocket 主动推给所有在线客户端
	go reminder.NewScheduler(s.deps.Reminders, time.Second, func(r reminder.Reminder) {
		// ③ 定时 Agent 任务：到点不是弹通知，而是跑一轮 Agent 把结果推给人
		if r.Kind == reminder.KindAgent {
			s.runAgentTask(ctx, r)
			return
		}
		s.deps.Hub.Broadcast(push.TypeReminder, map[string]any{
			"id":     r.ID,
			"title":  r.Title,
			"fireAt": r.FireAt.Format("2006-01-02 15:04:05"),
		})
		// 通知失败不影响推送本身：它只是额外的一条触达通道，
		// 不该因为系统通知服务不可用就让提醒整体失败。
		if s.deps.Notify != nil {
			if err := s.deps.Notify("⏰ 提醒："+r.Title, r.FireAt.Format("15:04")+" 到点"); err != nil {
				fmt.Fprintf(os.Stderr, "server: 系统通知失败: %v\n", err)
			}
		}
		fmt.Fprintf(os.Stderr, "server: 提醒已推送 id=%d %q\n", r.ID, r.Title)
	}).Run(ctx)

	srv := &http.Server{
		Addr:              s.deps.Cfg.HTTPAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "server: 监听 http://%s\n", s.deps.Cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// 优雅关闭：给在途请求一点时间收尾
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ---------------------------------------------------------------- 页面

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

// ---------------------------------------------------------------- 对话（SSE）

type chatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求体解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		http.Error(w, "message 不能为空", http.StatusBadRequest)
		return
	}

	// ---- 会话：空 id 则新建一个；带 id 则确保库里也有这一行 ----
	// 不再固定落到 "default"：每个页面会话都是独立的持久会话，
	// 刷新页面后从 localStorage 带回原 id 就能接着聊。
	sessID := req.SessionID
	if sessID == "" {
		sessID = chatsession.NewID()
	}
	s.ensurePersisted(sessID)

	// ---- 成本控制：超过预算直接拒绝，不再烧 token ----
	// ensureSession 会在内存没有该会话时从库里重建历史与累计 token，
	// 因此切到昨天的会话继续聊，上下文和预算控制都依然准确。
	sess := s.ensureSession(sessID)
	s.mu.Lock()
	if sess.tokens >= s.deps.Cfg.TokenBudget {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("本会话 token 已用尽（上限 %d），请开启新会话", s.deps.Cfg.TokenBudget),
			http.StatusTooManyRequests)
		return
	}
	history := append([]*schema.Message(nil), sess.history...)
	s.mu.Unlock()

	// ---- SSE 响应头 ----
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering: no 是给 Nginx 的指令，否则反向代理会缓冲住流式响应
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式输出", http.StatusInternalServerError)
		return
	}

	send := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	// 执行过程可视化（②）：工具调用步骤经回调转成 SSE "step" 事件实时推送。
	// emitter 随 ctx 下沉，全局注册的 StepHandler 由此找到推送目标；
	// 收集到的步骤最后随 assistant 消息落库。
	steps := newStepEmitter(send)
	ctx := withStepEmitter(r.Context(), steps)

	// 单请求总超时：这是「超时与取消」这条治理要求的落地。
	// 没有它，一次失控的会话会一直占着连接和 token。
	ctx, cancel := context.WithTimeout(ctx, s.deps.Cfg.RequestTimeout)
	defer cancel()

	// ---- 埋点：把本次会话的每一步记成 span ----
	if s.deps.Tracer != nil {
		ctx, _ = s.deps.Tracer.Start(ctx, sessID)
	}

	// ---- 记忆：长期摘要 + 短期窗口 ----
	messages := append(history, schema.UserMessage(req.Message))
	if s.deps.Memory != nil {
		prepared, err := s.deps.Memory.Prepare(ctx, sessID, messages)
		if err != nil {
			fmt.Fprintf(os.Stderr, "server: 组装记忆失败: %v\n", err)
		} else {
			messages = prepared
		}
	}

	stream, err := s.deps.Assistant.Stream(ctx, messages)
	if err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	defer stream.Close()

	var (
		full    strings.Builder
		promptT int
		complT  int
	)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				send("error", map[string]string{"error": "请求超时或已取消"})
			} else {
				send("error", map[string]string{"error": err.Error()})
			}
			s.finishTrace(ctx)
			return
		}
		if chunk == nil {
			continue
		}
		// 流式最后一个 chunk 才带完整用量，这里持续覆盖即可
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
			promptT = chunk.ResponseMeta.Usage.PromptTokens
			complT = chunk.ResponseMeta.Usage.CompletionTokens
		}
		if chunk.Content == "" {
			continue // 工具调用阶段的 chunk 没有正文
		}
		full.WriteString(chunk.Content)
		send("delta", map[string]string{"text": chunk.Content})
	}

	// 流式调用不会触发 OnEnd 回调，token 用量在这里回填
	if s.deps.Tracer != nil {
		s.deps.Tracer.AddUsage(ctx, promptT, complT)
	}
	s.finishTrace(ctx)

	// ---- 回写会话状态 ----
	s.mu.Lock()
	sess.tokens += promptT + complT
	sess.history = append(sess.history, schema.UserMessage(req.Message))
	sess.history = append(sess.history, &schema.Message{Role: schema.Assistant, Content: full.String()})
	if len(sess.history) > maxHistoryTurns*2 {
		sess.history = sess.history[len(sess.history)-maxHistoryTurns*2:]
	}
	tokens := sess.tokens
	s.mu.Unlock()

	// ---- 会话落库（① 持久化）----
	// 失败只记日志：聊天本身已经完成，不能因为库写不进去就让前端收到错误。
	// tool_calls 列存本轮的工具执行步骤（② 收集），当前前端历史回放还没用到，
	// 先落数据，渲染随时可加。
	s.persistTurn(sessID, req.Message, full.String(), promptT+complT, steps.json())

	send("done", map[string]any{
		"session_id": sessID,
		"text":       full.String(),
		"tokens":     map[string]int{"prompt": promptT, "completion": complT, "total": promptT + complT},
		"used":       tokens,
		"budget":     s.deps.Cfg.TokenBudget,
	})
}

func (s *Server) finishTrace(ctx context.Context) {
	if t := telemetry.TraceFrom(ctx); t != nil {
		t.Finish()
	}
}

// ---------------------------------------------------------------- 会话管理（① 持久化）

// ensurePersisted 保证库里存在这个会话行。
// 新建会话或用户带了一个库外的 id（如手改 localStorage）时补齐。
func (s *Server) ensurePersisted(id string) {
	if s.deps.ChatSessions == nil {
		return
	}
	if _, err := s.deps.ChatSessions.Get(id); err == nil {
		return
	} else if !errors.Is(err, chatsession.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "server: 查询会话失败: %v\n", err)
		return
	}
	if _, err := s.deps.ChatSessions.Create(id); err != nil {
		fmt.Fprintf(os.Stderr, "server: 创建会话记录失败: %v\n", err)
	}
}

// ensureSession 返回内存会话，不存在时从库里重建。
//
// 重建是本功能的关键：只把消息渲染给前端是不够的 —— 切换到旧会话后
// 继续提问，Agent 的上下文必须接着历史走，否则「恢复」只是个空壳。
func (s *Server) ensureSession(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess
	}

	sess := &session{}
	if s.deps.ChatSessions != nil {
		msgs, err := s.deps.ChatSessions.Messages(id, maxHistoryTurns*2)
		if err != nil && !errors.Is(err, chatsession.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "server: 读取会话历史失败: %v\n", err)
		}
		for _, m := range msgs {
			switch m.Role {
			case "user":
				sess.history = append(sess.history, schema.UserMessage(m.Content))
			case "assistant":
				if m.Content != "" {
					sess.history = append(sess.history,
						&schema.Message{Role: schema.Assistant, Content: m.Content})
				}
			}
		}
		// 累计 token 也要重建，否则换设备/重启后预算控制会归零重来
		if total, err := s.deps.ChatSessions.TotalTokens(id); err == nil {
			sess.tokens = total
		}
	}
	s.sessions[id] = sess
	return sess
}

// persistTurn 把一轮对话（用户消息 + 助手回复）写入会话库。
func (s *Server) persistTurn(sessionID, userText, assistantText string, tokens int, toolCalls string) {
	if s.deps.ChatSessions == nil {
		return
	}
	// 首条用户消息顺手当会话标题，前端侧边栏直接可用
	if err := s.deps.ChatSessions.Append(sessionID, "user", userText, "", 0, chatsession.Title(userText)); err != nil {
		fmt.Fprintf(os.Stderr, "server: 会话落库失败(user): %v\n", err)
		return
	}
	if err := s.deps.ChatSessions.Append(sessionID, "assistant", assistantText, toolCalls, tokens, ""); err != nil {
		fmt.Fprintf(os.Stderr, "server: 会话落库失败(assistant): %v\n", err)
	}
}

// handleSessionList 返回会话列表（侧边栏用），按最近活跃排序。
func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.deps.ChatSessions == nil {
		json.NewEncoder(w).Encode(map[string]any{"sessions": []any{}})
		return
	}
	list, err := s.deps.ChatSessions.List(100)
	if err != nil {
		http.Error(w, "读取会话列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type item struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Updated string `json:"updated_at"`
	}
	items := make([]item, 0, len(list))
	for _, sess := range list {
		title := sess.Title
		if title == "" {
			title = "（空会话）"
		}
		items = append(items, item{
			ID:      sess.ID,
			Title:   title,
			Updated: sess.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"sessions": items})
}

// handleSessionMessages 返回会话历史，并顺带重建内存会话上下文。
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 id", http.StatusBadRequest)
		return
	}

	// 读历史前先 ensure：万一是新 id，内存上下文就此建立，
	// 之后在这个会话里发消息，Agent 直接接着历史走。
	s.ensurePersisted(id)
	s.ensureSession(id)

	if s.deps.ChatSessions == nil {
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
		return
	}
	msgs, err := s.deps.ChatSessions.Messages(id, 0)
	if err != nil {
		http.Error(w, "读取会话历史失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type item struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		Tokens  int    `json:"tokens"`
	}
	items := make([]item, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, item{Role: m.Role, Content: m.Content, Tokens: m.Tokens})
	}
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "messages": items})
}

// handleSessionDelete 删除会话：库里的消息与内存里的上下文一起清。
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 id", http.StatusBadRequest)
		return
	}
	if s.deps.ChatSessions != nil {
		if err := s.deps.ChatSessions.Delete(id); err != nil {
			http.Error(w, "删除会话失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 辅助接口

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"tools": s.deps.Assistant.ToolNames(),
	})
}

// handleTrace 暴露最近一次会话的 span 链，用于排查「它为什么走了这条路」。
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		sessionID = "default"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.deps.Tracer == nil {
		json.NewEncoder(w).Encode(map[string]any{"error": "未启用埋点"})
		return
	}
	tr := s.deps.Tracer.Get(sessionID)
	if tr == nil {
		json.NewEncoder(w).Encode(map[string]any{"error": "没有该会话的 trace"})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": tr.SessionID,
		"spans":      tr.Spans(),
		"text":       tr.AsText(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	s.mu.Lock()
	sessions := len(s.sessions)
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"sessions": sessions,
		"pushConn": s.deps.Hub.ConnCount(),
		"tools":    len(s.deps.Assistant.ToolNames()),
	})
}
