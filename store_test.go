package reminder

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openOldSchema 按「迁移前」的旧结构建库，用于测试迁移逻辑。
func openOldSchema(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`CREATE TABLE reminder (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			title       TEXT NOT NULL,
			fire_at     DATETIME NOT NULL,
			status      TEXT NOT NULL DEFAULT 'pending',
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAddAndClaimDue(t *testing.T) {
	s := open(t)

	// 一条已到点、一条未到点
	if _, err := s.Add("已到点", time.Now().Add(-time.Minute), ""); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if _, err := s.Add("未到点", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	due, err := s.ClaimDue(time.Now(), 10)
	if err != nil {
		t.Fatalf("ClaimDue 失败: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("期望领取 1 条，实际 %d：%+v", len(due), due)
	}
	if due[0].Title != "已到点" {
		t.Errorf("领取到 %q，期望「已到点」", due[0].Title)
	}
}

// TestClaimDueIsIdempotent 是最关键的一条：
// 第二次领取必须为空，否则重启或双实例会导致同一条提醒被推两次。
func TestClaimDueIsIdempotent(t *testing.T) {
	s := open(t)
	if _, err := s.Add("喝水", time.Now().Add(-time.Second), ""); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	first, err := s.ClaimDue(time.Now(), 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("首次领取失败: %v, %d 条", err, len(first))
	}
	second, err := s.ClaimDue(time.Now(), 10)
	if err != nil {
		t.Fatalf("二次领取失败: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("二次领取到了 %d 条 —— 会导致重复推送", len(second))
	}
}

func TestCancelOnlyPending(t *testing.T) {
	s := open(t)
	r, err := s.Add("待取消", time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if err := s.Cancel(r.ID); err != nil {
		t.Errorf("取消待触发提醒失败: %v", err)
	}
	// 已取消的不能再取消一次
	if err := s.Cancel(r.ID); err == nil {
		t.Error("重复取消应当报错，实际成功了")
	}
}

func TestRejectsEmptyAndZeroTime(t *testing.T) {
	s := open(t)
	if _, err := s.Add("", time.Now().Add(time.Hour), ""); err == nil {
		t.Error("空标题应当被拒绝")
	}
	if _, err := s.Add("没时间", time.Time{}, ""); err == nil {
		t.Error("零时间应当被拒绝")
	}
}

func TestListFilter(t *testing.T) {
	s := open(t)
	if _, err := s.Add("A", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("B", time.Now().Add(2*time.Hour), ""); err != nil {
		t.Fatal(err)
	}

	all, err := s.List("", 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("全部列出失败: %v, %d 条", err, len(all))
	}
	pending, _ := s.List(StatusPending, 10)
	if len(pending) != 2 {
		t.Errorf("pending 过滤后应为 2 条，实际 %d", len(pending))
	}
	fired, _ := s.List(StatusFired, 10)
	if len(fired) != 0 {
		t.Errorf("fired 过滤后应为 0 条，实际 %d", len(fired))
	}
}

// TestSchedulerFires 验证调度器确实会回调。
func TestSchedulerFires(t *testing.T) {
	s := open(t)
	if _, err := s.Add("调度器测试", time.Now().Add(-time.Second), ""); err != nil {
		t.Fatal(err)
	}

	got := make(chan Reminder, 4)
	sc := NewScheduler(s, 10*time.Millisecond, func(r Reminder) { got <- r })
	go sc.Run(t.Context()) // t.Context() 在测试结束时自动取消，调度循环随之退出

	select {
	case r := <-got:
		if r.Title != "调度器测试" {
			t.Errorf("回调收到 %q，期望「调度器测试」", r.Title)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("调度器在 3 秒内没有触发回调")
	}
}

// TestAgentTaskKindAndResult 验证 ③ 主动式 Agent 的两个新行为：
// prompt 非空 => kind=agent；执行结果可回写并读出。
func TestAgentTaskKindAndResult(t *testing.T) {
	s := open(t)

	plain, err := s.Add("普通提醒", time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Kind != KindReminder {
		t.Errorf("无 prompt 时 kind 应为 %q，得到 %q", KindReminder, plain.Kind)
	}

	task, err := s.Add("晨报", time.Now().Add(time.Hour), "总结我今天的待办")
	if err != nil {
		t.Fatal(err)
	}
	if task.Kind != KindAgent {
		t.Errorf("有 prompt 时 kind 应为 %q，得到 %q", KindAgent, task.Kind)
	}
	if task.Prompt != "总结我今天的待办" {
		t.Errorf("prompt 未正确保存: %q", task.Prompt)
	}

	// 执行完成后回写结果
	if err := s.FinishAgentTask(task.ID, "今天有 3 件待办…"); err != nil {
		t.Fatalf("FinishAgentTask 失败: %v", err)
	}
	got, err := s.findByID(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != "今天有 3 件待办…" {
		t.Errorf("结果未回写: %q", got.Result)
	}
	if got.FinishedAt == "" {
		t.Error("完成时间应为非空")
	}
}

// TestClaimDueCarriesAgentFields 确保领取时新列一起带出来
// （onFire 分支靠 Kind 决定走提醒还是跑 Agent）。
func TestClaimDueCarriesAgentFields(t *testing.T) {
	s := open(t)
	if _, err := s.Add("到点任务", time.Now().Add(-time.Second), "跑一下"); err != nil {
		t.Fatal(err)
	}
	due, err := s.ClaimDue(time.Now(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("ClaimDue: %v, %d 条", err, len(due))
	}
	if due[0].Kind != KindAgent || due[0].Prompt != "跑一下" {
		t.Errorf("领取结果缺少 agent 字段: %+v", due[0])
	}
}

// TestMigrateExistingDB 模拟老库：先按旧结构建表插入数据，
// 再 Open（触发迁移），老数据必须还在且新列有默认值。
func TestMigrateExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// 手工建一张「旧版」reminder 表（没有 kind/prompt/result/finished_at）
	oldDB := openOldSchema(t, path)
	if _, err := oldDB.Exec(
		`INSERT INTO reminder (title, fire_at) VALUES (?, ?)`,
		"老数据", time.Now().Add(time.Hour).UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	oldDB.Close()

	// 重新 Open 触发迁移
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	defer s.Close()

	list, err := s.List("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "老数据" {
		t.Fatalf("老数据丢失: %+v", list)
	}
	if list[0].Kind != KindReminder {
		t.Errorf("老数据迁移后 kind 应为默认 %q，得到 %q", KindReminder, list[0].Kind)
	}

	// 迁移后的库要能正常写新类型
	if _, err := s.Add("新任务", time.Now().Add(time.Hour), "prompt"); err != nil {
		t.Fatal(err)
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "reminder.db"))
	if err != nil {
		t.Fatalf("打开提醒库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
