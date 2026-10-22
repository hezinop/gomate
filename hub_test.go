package push

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsURL 把 http:// 换成 ws://
func wsURL(base string) string { return "ws" + strings.TrimPrefix(base, "http") }

func TestBroadcastAssignsIncreasingSeq(t *testing.T) {
	h := New(16)
	if s1 := h.Broadcast(TypeNotice, "a"); s1 != 1 {
		t.Errorf("第一条 seq = %d，期望 1", s1)
	}
	if s2 := h.Broadcast(TypeNotice, "b"); s2 != 2 {
		t.Errorf("第二条 seq = %d，期望 2", s2)
	}
	if h.CurrentSeq() != 2 {
		t.Errorf("CurrentSeq = %d，期望 2", h.CurrentSeq())
	}
}

// TestSinceReplaysOnlyNewer 验证补发只给"比 last_seq 新"的消息，
// 这是断线重连不丢消息的依据。
func TestSinceReplaysOnlyNewer(t *testing.T) {
	h := New(16)
	for i := 0; i < 5; i++ {
		h.Broadcast(TypeNotice, i)
	}

	got := h.Since(2)
	if len(got) != 3 {
		t.Fatalf("Since(2) 返回 %d 条，期望 3 条（seq 3,4,5）", len(got))
	}
	if got[0].Seq != 3 {
		t.Errorf("首条 seq = %d，期望 3", got[0].Seq)
	}
}

func TestBufferIsCapped(t *testing.T) {
	h := New(4)
	for i := 0; i < 20; i++ {
		h.Broadcast(TypeNotice, i)
	}
	if got := h.Since(0); len(got) != 4 {
		t.Errorf("缓冲上限 4，却保留了 %d 条", len(got))
	}
}

// TestWebSocketReceivesAndReplays 是端到端验证：
// 客户端断线后带上 last_seq 重连，应当收到断线期间漏掉的消息。
func TestWebSocketReceivesAndReplays(t *testing.T) {
	h := New(64)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	defer srv.Close()

	// 先广播两条，此时还没有客户端
	h.Broadcast(TypeReminder, map[string]string{"title": "第一条"})
	last := h.Broadcast(TypeReminder, map[string]string{"title": "第二条"})

	// 带着 last_seq 重连 —— 应当补发这之后的消息
	conn, _, err := websocket.DefaultDialer.Dial(
		wsURL(srv.URL)+"/api/ws?last_seq="+itoa(last-1), nil)
	if err != nil {
		t.Fatalf("连接 WebSocket 失败: %v", err)
	}
	defer conn.Close()

	msg := readMessage(t, conn)
	if msg.Type != TypeReminder {
		t.Errorf("收到类型 %q，期望 reminder", msg.Type)
	}
	if msg.Seq != last {
		t.Errorf("补发的 seq = %d，期望 %d", msg.Seq, last)
	}

	// 在线期间的新消息应当直接到达
	newSeq := h.Broadcast(TypeNotice, "在线消息")
	msg = readMessage(t, conn)
	if msg.Seq != newSeq {
		t.Errorf("在线消息 seq = %d，期望 %d", msg.Seq, newSeq)
	}

	if n := h.ConnCount(); n != 1 {
		t.Errorf("ConnCount = %d，期望 1", n)
	}
}

func readMessage(t *testing.T, conn *websocket.Conn) Message {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m Message
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	return m
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
