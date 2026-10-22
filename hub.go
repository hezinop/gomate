// Package push 实现服务端的主动推送通道（WebSocket + seq + 断线补发）。
//
// 为什么需要它：多数 Agent 只有「一问一答」这一种交互，
// 但真实场景里服务端需要主动找用户 —— 提醒到点、后台长任务完成。
// 这条通道就是为此存在的。
//
// 可靠性设计（与 IM 项目的会话 seq 是同一套思路）：
//   - 每条消息带**单调递增 seq**
//   - 服务端保留最近 N 条的环形缓冲
//   - 客户端断线重连时带上 last_seq，服务端把缺失的部分补发（补洞）
//   - 心跳保活，及时清理死连接
//
// 这三件事解决的是三个不同的失败：丢消息、乱序、连接假死。
package push

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 消息类型。
const (
	TypeReminder = "reminder" // 提醒到点
	TypeNotice   = "notice"   // 一般性通知
	TypePing     = "ping"     // 心跳
	TypeAgentTask = "agent_task" // 定时 Agent 任务完成（③ 主动式 Agent）
)

// Message 是推送消息。Seq 全局单调递增，是补洞的依据。
type Message struct {
	Seq     int64     `json:"seq"`
	Type    string    `json:"type"`
	Payload any       `json:"payload"`
	At      time.Time `json:"at"`
}

// 默认参数。
const (
	defaultBufferSize = 128 // 保留最近多少条用于补发
	clientChanSize    = 32  // 单连接的发送缓冲
	pingInterval      = 20 * time.Second
	writeWait         = 10 * time.Second
)

var upgrader = websocket.Upgrader{
	// 单人本地部署，允许同源即可；生产环境应改成显式白名单。
	// 不校验 Origin 会让任意站点都能连上你的助理并收到推送。
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Hub 是推送中心。
type Hub struct {
	mu      sync.RWMutex
	seq     int64
	buf     []Message
	bufSize int
	conns   map[*client]struct{}
}

type client struct {
	id string
	ch chan Message
}

// New 创建 Hub。bufferSize 决定断线后最多能补多少条。
func New(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}
	return &Hub{
		bufSize: bufferSize,
		conns:   make(map[*client]struct{}),
	}
}

// Broadcast 广播一条消息，返回它分配到的 seq。
//
// 发送是**非阻塞**的：某个客户端消费慢或已假死时直接丢弃并记日志，
// 绝不因为一个慢连接拖住整个广播（进而拖住提醒调度循环）。
func (h *Hub) Broadcast(typ string, payload any) int64 {
	h.mu.Lock()
	h.seq++
	msg := Message{Seq: h.seq, Type: typ, Payload: payload, At: time.Now()}

	h.buf = append(h.buf, msg)
	if len(h.buf) > h.bufSize {
		h.buf = h.buf[len(h.buf)-h.bufSize:]
	}

	for c := range h.conns {
		select {
		case c.ch <- msg:
		default:
			fmt.Fprintf(os.Stderr, "push: 客户端 %s 消费过慢，丢弃 seq=%d\n", c.id, h.seq)
		}
	}
	seq := h.seq
	h.mu.Unlock()
	return seq
}

// Since 返回 seq 大于 after 的历史消息，供重连补发。
func (h *Hub) Since(after int64) []Message {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []Message
	for _, m := range h.buf {
		if m.Seq > after {
			out = append(out, m)
		}
	}
	return out
}

// CurrentSeq 返回当前最新 seq。
func (h *Hub) CurrentSeq() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.seq
}

// ConnCount 返回活跃连接数，用于健康检查。
func (h *Hub) ConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

func (h *Hub) subscribe(id string) *client {
	c := &client{id: id, ch: make(chan Message, clientChanSize)}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// ServeWS 处理 WebSocket 连接。
//
// 客户端应在 URL 上带 last_seq=<上次收到的 seq>：
// 服务端会把这之后的所有消息补发一遍，实现断线重连不丢消息。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push: 升级 WebSocket 失败: %v\n", err)
		return
	}
	defer conn.Close()

	var lastSeq int64
	if v := r.URL.Query().Get("last_seq"); v != "" {
		fmt.Sscanf(v, "%d", &lastSeq)
	}

	id := r.RemoteAddr
	c := h.subscribe(id)
	defer h.unsubscribe(c)

	// ---- 1. 补发断线期间漏掉的消息 ----
	missed := h.Since(lastSeq)
	if len(missed) > 0 {
		fmt.Fprintf(os.Stderr, "push: 客户端 %s 重连，补发 %d 条（last_seq=%d）\n", id, len(missed), lastSeq)
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 读协程：只用来感知客户端断开。客户端不发业务消息，
	// 但仍要处理控制帧（pong / close），否则连接会僵死。
	go func() {
		defer cancel()
		conn.SetReadDeadline(time.Now().Add(pingInterval * 3))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(pingInterval * 3))
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	send := func(m Message) bool {
		if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
			return false
		}
		data, err := json.Marshal(m)
		if err != nil {
			return true // 序列化失败跳过这条，不断开连接
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return false
		}
		return true
	}

	for _, m := range missed {
		if !send(m) {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.ch:
			if !send(m) {
				return
			}
		case <-ticker.C:
			// 心跳：既是保活，也让客户端能确认自己还连着
			if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
