// Package memory 实现双层记忆：短期窗口 + 长期摘要。
//
// 为什么必须自己写：Eino 不内置记忆（和 LangChain 最大的差异），
// 因为生产场景的记忆策略（窗口裁剪、摘要压缩、跨会话检索）必须由业务定，
// 框架内置的反而是坑。
//
// 本包的策略：
//   - 短期：按 **token 预算** 滑动裁剪，而不是按轮数。
//     按轮数裁剪是错的 —— 一轮闲聊和一轮塞进整篇文档，长度能差两个数量级。
//   - 长期：被挤出窗口的历史，用一次额外的 LLM 调用压缩成摘要并持久化；
//     后续对话把摘要塞回 system prompt，实现"跨会话还记得"。
package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/cloudwego/eino/schema"
	_ "modernc.org/sqlite"
)

// EstimateTokens 估算一段文本的 token 数。
//
// 精确分词需要 tokenizer，代价过高；这里用字符级启发式：
//   - 中日韩字符：约 1 字 1 token
//   - 其余字符：约 4 字符 1 token（英文单词的典型比例）
//
// 估算偏大会让窗口偏保守，是安全的方向 —— 宁可少带历史，也不要撑爆上下文。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var cjk, other int
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4 + 1
}

// MessagesTokens 估算一组消息的总 token，含每条消息的角色开销。
func MessagesTokens(msgs []*schema.Message) int {
	total := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		total += EstimateTokens(m.Content) + 4 // +4 近似 role/分隔的开销
	}
	return total
}

// ---------------------------------------------------------------- 摘要持久化

// Store 持久化会话摘要。
type Store struct {
	db *sql.DB
}

// Summary 是一条会话的长期摘要。
type Summary struct {
	SessionID string
	Content   string
	UpdatedAt time.Time
}

// Open 打开（必要时创建）摘要库。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS summary (
		session_id TEXT PRIMARY KEY,
		content    TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	return nil
}

// Get 读取会话摘要；不存在返回空串。
func (s *Store) Get(sessionID string) (string, error) {
	var content string
	err := s.db.QueryRow(`SELECT content FROM summary WHERE session_id = ?`, sessionID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return content, nil
}

// Save 写入（覆盖）会话摘要。
func (s *Store) Save(sessionID, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO summary (session_id, content, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
		sessionID, content, time.Now().Format("2006-01-02 15:04:05"))
	return err
}

// ---------------------------------------------------------------- 记忆管理器

// Summarizer 是注入的压缩函数。之所以用函数注入而不是直接依赖 agent 包，
// 是为了避免 import 环，也让单测可以用假实现替换掉真实 LLM 调用。
type Summarizer func(ctx context.Context, evicted []*schema.Message, prev string) (string, error)

// Manager 管理短期窗口与长期摘要。
type Manager struct {
	store     *Store
	budget    int // 短期窗口的 token 预算
	summarize Summarizer
}

// New 创建 Manager。budget 为短期窗口的 token 预算（建议 2000–4000）。
func New(store *Store, budget int, summarize Summarizer) *Manager {
	if budget <= 0 {
		budget = 3000
	}
	return &Manager{store: store, budget: budget, summarize: summarize}
}

// Trim 按 token 预算从**头部**丢弃最旧的消息，保留最近的内容。
// 被丢弃的部分返回给调用方，供压缩成摘要使用。
//
// 注意一定保留偶数边界：一条 user 消息后面必须跟着它的 assistant 回复，
// 否则模型会看到"用户问了但没回答"的残缺上下文，容易接着上一轮乱答。
func Trim(msgs []*schema.Message, budget int) (kept, evicted []*schema.Message) {
	if len(msgs) == 0 {
		return nil, nil
	}
	total := MessagesTokens(msgs)
	if total <= budget {
		return msgs, nil
	}

	// 从前往后丢，直到总 token 落入预算
	drop := 0
	for drop < len(msgs) && total > budget {
		total -= EstimateTokens(msgs[drop].Content) + 4
		drop++
	}
	// 修正到问答对边界：只在 user 消息处切
	for drop < len(msgs) && msgs[drop].Role != schema.User {
		drop++
	}
	if drop >= len(msgs) {
		// 极端情况：单轮就超预算，只能保留最后一条
		drop = len(msgs) - 1
	}
	return msgs[drop:], msgs[:drop]
}

// Prepare 组装真正送给模型的消息：
// 长期摘要（若有）作为 system 前缀 + 短期窗口内的历史。
//
// 压缩是「机会性」的：只有确实挤出了历史、且配置了 summarizer 时才触发，
// 避免每次对话都额外烧一次 LLM 调用。
func (m *Manager) Prepare(ctx context.Context, sessionID string, history []*schema.Message) ([]*schema.Message, error) {
	kept, evicted := Trim(history, m.budget)

	if len(evicted) > 0 && m.summarize != nil {
		prev := ""
		if m.store != nil {
			prev, _ = m.store.Get(sessionID)
		}
		summary, err := m.summarize(ctx, evicted, prev)
		if err != nil {
			// 压缩失败不阻断对话 —— 退化成"丢掉更老的历史"，
			// 但要把失败暴露出来，便于排查
			fmt.Fprintf(os.Stderr, "memory: 生成摘要失败，本次跳过长期记忆: %v\n", err)
		} else if m.store != nil {
			if err := m.store.Save(sessionID, summary); err != nil {
				fmt.Fprintf(os.Stderr, "memory: 保存摘要失败: %v\n", err)
			}
		}
	}

	out := make([]*schema.Message, 0, len(kept)+1)
	if m.store != nil {
		if summary, err := m.store.Get(sessionID); err == nil && summary != "" {
			out = append(out, schema.SystemMessage(
				"以下是你与用户更早对话的压缩摘要，用于保持上下文连贯：\n"+summary))
		}
	}
	out = append(out, kept...)
	return out, nil
}
