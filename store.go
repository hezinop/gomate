// Package reminder 实现定时提醒的持久化与触发调度。
//
// 设计约束：
//  1. **进程重启不能丢** —— 提醒落 SQLite，而不是只放在内存定时器里
//  2. **不能重复触发** —— 领取（claim）动作在事务里完成并立刻改写状态，
//     避免多实例或重启后同一条提醒被推两次
//  3. 调度器只负责「到点」，推送动作由调用方注入（onFire），
//     这样它可以被 HTTP 服务、CLI、测试分别复用
//  4. **两种任务**（③ 主动式 Agent）：
//     - kind=reminder：到点弹一条通知（原行为）
//     - kind=agent：到点自动跑一轮 Agent 执行 prompt，把结果推送给人
//     两种任务共用同一张表和同一套调度，差别只在 onFire 分支
package reminder

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver name: "sqlite"
)

// 提醒状态。
const (
	StatusPending   = "pending"
	StatusFired     = "fired"
	StatusCancelled = "cancelled"
)

// 任务类型（③ 主动式 Agent）。
const (
	// KindReminder 是普通提醒：到点弹一条通知。
	KindReminder = "reminder"
	// KindAgent 是定时 Agent 任务：到点自动执行 prompt 并推送结果。
	KindAgent = "agent"
)

// Reminder 是一条定时提醒（或定时 Agent 任务）。
type Reminder struct {
	ID        int64
	Title     string
	FireAt    time.Time
	Status    string
	CreatedAt time.Time

	// 以下字段为 ③ 主动式 Agent 新增。普通提醒下为零值。
	Kind       string // reminder / agent
	Prompt     string // agent 任务的执行指令
	Result     string // agent 任务的执行结果（完成后回写）
	FinishedAt string // 完成时间（本地时间字符串），未完成为空
}

// Store 封装提醒库。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）提醒库并完成建表。
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
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS reminder (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			title       TEXT NOT NULL,
			fire_at     DATETIME NOT NULL,
			status      TEXT NOT NULL DEFAULT 'pending',
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reminder_due ON reminder (status, fire_at)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("初始化表结构失败: %w", err)
		}
	}
	return s.migrate()
}

// migrate 给老库追加 ③ 主动式 Agent 需要的列。
//
// 用 ALTER TABLE ADD COLUMN 而不是改 CREATE TABLE：用户的 var/reminder.db
// 早就存在，不能为了加列把数据清了。SQLite 的 ADD COLUMN 带默认值时不重写表，
// 代价可忽略；列已存在时忽略 "duplicate column" 错误。
func (s *Store) migrate() error {
	cols := []string{
		`ALTER TABLE reminder ADD COLUMN kind TEXT NOT NULL DEFAULT '` + KindReminder + `'`,
		`ALTER TABLE reminder ADD COLUMN prompt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE reminder ADD COLUMN result TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE reminder ADD COLUMN finished_at DATETIME`,
	}
	for _, c := range cols {
		if _, err := s.db.Exec(c); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("迁移 reminder 表失败: %w", err)
		}
	}
	return nil
}

// Add 创建一条提醒。fireAt 必须是将来的时间。
// prompt 非空时创建为 agent 任务（到点自动执行），为空则是普通提醒。
func (s *Store) Add(title string, fireAt time.Time, prompt string) (Reminder, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Reminder{}, fmt.Errorf("提醒内容不能为空")
	}
	if fireAt.IsZero() {
		return Reminder{}, fmt.Errorf("提醒时间不能为空")
	}
	// 存 UTC，取的时候再转本地 —— 避免夏令时/时区切换导致的时间漂移
	kind := KindReminder
	if strings.TrimSpace(prompt) != "" {
		kind = KindAgent
	}
	res, err := s.db.Exec(
		`INSERT INTO reminder (title, fire_at, prompt, kind) VALUES (?, ?, ?, ?)`,
		title, fireAt.UTC().Format("2006-01-02 15:04:05"), prompt, kind)
	if err != nil {
		return Reminder{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Reminder{}, err
	}
	return s.findByID(id)
}

// selectCols 是所有查询共用的列清单，新增列只改这一处。
// 时间一律用 strftime(..., 'localtime') 取出后再解析，
// 与 Add 存 UTC 的做法配对，避免驱动时区差异。
const selectCols = `SELECT id, title,
	strftime('%Y-%m-%d %H:%M:%S', fire_at, 'localtime'),
	status,
	strftime('%Y-%m-%d %H:%M:%S', created_at, 'localtime'),
	kind, prompt, result,
	strftime('%Y-%m-%d %H:%M:%S', finished_at, 'localtime')
	FROM reminder`

// List 列出提醒。status 为空表示不过滤。
func (s *Store) List(status string, limit int) ([]Reminder, error) {
	if limit <= 0 {
		limit = 20
	}
	query := selectCols
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY fire_at ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Reminder
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimDue 领取所有到点的提醒并立刻标记为已触发。
//
// 领取与改状态放在同一个事务里，是防重复推送的关键：
// 若分成两步，进程在中间崩溃或第二个实例并发轮询，同一条提醒就会推两次。
func (s *Store) ClaimDue(now time.Time, limit int) ([]Reminder, error) {
	if limit <= 0 {
		limit = 10
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		selectCols+`
		 WHERE status = ? AND fire_at <= ?
		 ORDER BY fire_at ASC LIMIT ?`,
		StatusPending, now.UTC().Format("2006-01-02 15:04:05"), limit)
	if err != nil {
		return nil, err
	}

	var (
		out []Reminder
		ids []int64
	)
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE reminder SET status = ? WHERE id = ?`, StatusFired, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Cancel 取消一条还未触发的提醒。
func (s *Store) Cancel(id int64) error {
	res, err := s.db.Exec(
		`UPDATE reminder SET status = ? WHERE id = ? AND status = ?`,
		StatusCancelled, id, StatusPending)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("没有找到 id=%d 的待触发提醒", id)
	}
	return nil
}

// FinishAgentTask 回写 agent 任务的执行结果。
// 任务在 ClaimDue 时已被标记为 fired（防重），这里只补结果，
// 因此崩溃窗口内「已触发但没跑完」的任务不会重跑 —— 与普通提醒的取舍一致。
func (s *Store) FinishAgentTask(id int64, result string) error {
	_, err := s.db.Exec(
		`UPDATE reminder SET result = ?, finished_at = CURRENT_TIMESTAMP WHERE id = ?`,
		result, id)
	return err
}

func (s *Store) findByID(id int64) (Reminder, error) {
	row := s.db.QueryRow(selectCols+` WHERE id = ?`, id)
	r, err := scanRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Reminder{}, fmt.Errorf("没有找到 id=%d 的提醒", id)
		}
		return Reminder{}, err
	}
	return r, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanRow(s scanner) (Reminder, error) {
	var (
		r         Reminder
		fireAt    string
		createdAt string
		finished  sql.NullString
	)
	if err := s.Scan(&r.ID, &r.Title, &fireAt, &r.Status, &createdAt,
		&r.Kind, &r.Prompt, &r.Result, &finished); err != nil {
		return Reminder{}, err
	}
	if v, err := time.ParseInLocation("2006-01-02 15:04:05", fireAt, time.Local); err == nil {
		r.FireAt = v
	} else {
		return Reminder{}, fmt.Errorf("解析触发时间失败: %w", err)
	}
	if v, err := time.ParseInLocation("2006-01-02 15:04:05", createdAt, time.Local); err == nil {
		r.CreatedAt = v
	}
	r.FinishedAt = finished.String // 未完成为空串，scan 进来是 NULL
	return r, nil
}

// ---------------------------------------------------------------- 调度器

// Scheduler 周期性轮询到点的提醒并回调。
//
// 为什么用轮询而不是内存定时器（time.AfterFunc）：
// 内存定时器在进程重启后会全部丢失，而提醒是用户资产，不能丢。
// 代价是最长有一个轮询间隔的延迟（默认 1s），对提醒场景完全够用。
type Scheduler struct {
	store    *Store
	onFire   func(Reminder)
	interval time.Duration
}

// NewScheduler 创建调度器。onFire 由调用方注入推送动作。
func NewScheduler(store *Store, interval time.Duration, onFire func(Reminder)) *Scheduler {
	if interval <= 0 {
		interval = time.Second
	}
	return &Scheduler{store: store, interval: interval, onFire: onFire}
}

// Run 阻塞运行，直到 ctx 取消。
func (sc *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(sc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sc.tick(ctx)
		}
	}
}

func (sc *Scheduler) tick(ctx context.Context) {
	due, err := sc.store.ClaimDue(time.Now(), 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reminder: 领取到期提醒失败: %v\n", err)
		return
	}
	for _, r := range due {
		if ctx.Err() != nil {
			return
		}
		// 单条回调 panic 不能拖垮整个调度循环
		func() {
			defer func() {
				if p := recover(); p != nil {
					fmt.Fprintf(os.Stderr, "reminder: 回调 panic: %v\n", p)
				}
			}()
			sc.onFire(r)
		}()
	}
}
