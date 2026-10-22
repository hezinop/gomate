package memostore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/chenxiaowei/gomate/internal/embedding"
)

// fakeEmbedder 按关键词映射到固定向量，让语义检索的结果可预测：
//
//	「数据库性能」-> A 方向（与「连接池调优」同向）
//	「缓存」      -> B 方向
//	其余          -> C 方向（与 A、B 正交）
type fakeEmbedder struct {
	dim int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	switch {
	case strings.Contains(text, "数据库性能"), strings.Contains(text, "连接池调优"):
		return []float32{1, 0, 0, 0}, nil
	case strings.Contains(text, "缓存"):
		return []float32{0, 1, 0, 0}, nil
	default:
		return []float32{0, 0, 1, 0}, nil
	}
}

func (f *fakeEmbedder) Dim() int { return f.dim }

var _ embedding.Embedder = (*fakeEmbedder)(nil)

// openWithFake 打开带假向量化器的库。
func openWithFake(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:", WithEmbedder(&fakeEmbedder{dim: 4}))
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestHybridSearchFindsSynonym 是 ④ 的核心场景：
// 笔记里完全没有「数据库性能」四个字，关键词检索必然失败，
// 只有向量语义这一路能把它找回来。
func TestHybridSearchFindsSynonym(t *testing.T) {
	s := openWithFake(t)
	if _, err := s.Add("连接池调优笔记：连接池大小直接影响查询响应"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("今天天气不错，去公园散步"); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search("数据库性能", 5)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("混合检索应命中 1 条，得到 %d 条: %+v", len(hits), hits)
	}
	if !strings.Contains(hits[0].Content, "连接池调优") {
		t.Errorf("命中的应是连接池笔记，得到 %q", hits[0].Content)
	}
}

// TestSearchWithoutEmbedderIsFTSOnly 证明上面那条真的是向量路找回来的：
// 同样的数据，不配 embedder 时应当检索不到（关键词路失败）。
func TestSearchWithoutEmbedderIsFTSOnly(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Add("连接池调优笔记：连接池大小直接影响查询响应"); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search("数据库性能", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("无 embedder 时应走纯关键词（找不到同义词），得到 %d 条", len(hits))
	}
}

// TestHybridFallsBackWhenEmbedFails：向量化失败时退回关键词，不报错。
func TestHybridFallsBackWhenEmbedFails(t *testing.T) {
	s := openWithFake(t)
	if _, err := s.Add("连接池调优笔记"); err != nil {
		t.Fatal(err)
	}
	// 换一个必然失败的 embedder
	s.emb = failingEmbedder{}

	hits, err := s.Search("连接池调优", 5)
	if err != nil {
		t.Fatalf("向量失败不应让检索报错: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("应退回关键词检索并命中，得到 %d 条", len(hits))
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errEmbedFailed
}
func (failingEmbedder) Dim() int { return 0 }

var errEmbedFailed = &embedError{"模拟的向量化失败"}

type embedError struct{ s string }

func (e *embedError) Error() string { return e.s }

// TestKeywordHitStillWins 验证混合检索不劣化纯关键词场景：
// 关键词精确命中时，该条必须排在结果里（两路都加分）。
func TestKeywordHitStillWins(t *testing.T) {
	s := openWithFake(t)
	if _, err := s.Add("分布式缓存的一致性哈希方案"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("连接池调优笔记"); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search("分布式缓存", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Content, "一致性哈希") {
		t.Fatalf("关键词精确命中应排第一，得到 %+v", hits)
	}
}

// TestRRFFuse 直接验证融合逻辑：两路都命中的 id 融合分最高。
func TestRRFFuse(t *testing.T) {
	a := []Memo{{ID: 1}, {ID: 2}}
	b := []Memo{{ID: 2}, {ID: 3}}
	got := rrfFuse(a, b, 10)
	if len(got) != 3 {
		t.Fatalf("应融合出 3 条，得到 %d", len(got))
	}
	if got[0].ID != 2 {
		t.Errorf("两路都命中的 id=2 应排第一，得到 %d", got[0].ID)
	}
	// 单路第一（1/(60+1)）应胜过单路第二（1/(60+2)）
	if got[1].ID != 1 || got[2].ID != 3 {
		t.Errorf("单路结果应按各自排名排，得到 %d, %d", got[1].ID, got[2].ID)
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"同向为 1", []float32{1, 0}, []float32{2, 0}, 1},
		{"正交为 0", []float32{1, 0}, []float32{0, 1}, 0},
		{"反向为 -1", []float32{1, 0}, []float32{-3, 0}, -1},
		{"零向量为 0", []float32{0, 0}, []float32{1, 1}, 0},
		{"长度不等为 0", []float32{1}, []float32{1, 2}, 0},
	}
	for _, c := range cases {
		if got := cosine(c.a, c.b); got != c.want {
			t.Errorf("%s: cosine = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

func TestVecEncodeDecodeRoundTrip(t *testing.T) {
	vec := []float32{0.5, -1.25, 3.14159, 0}
	got := decodeVec(encodeVec(vec))
	if len(got) != len(vec) {
		t.Fatalf("往返后长度变了: %d vs %d", len(got), len(vec))
	}
	for i := range vec {
		if got[i] != vec[i] {
			t.Errorf("第 %d 个元素往返后变了: %v vs %v", i, got[i], vec[i])
		}
	}
	if decodeVec(nil) != nil || decodeVec([]byte{1, 2, 3}) != nil {
		t.Error("空或非 4 倍长的字节流应解出 nil")
	}
}

// TestBackfill 验证老笔记补算向量。
func TestBackfill(t *testing.T) {
	// 先不带 embedder 写两条（模拟老库数据）
	path := filepath.Join(t.TempDir(), "memo.db")
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Add("连接池调优笔记"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Add("分布式缓存方案"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	// 带 embedder 重开并补算
	s, err := Open(path, WithEmbedder(&fakeEmbedder{dim: 4}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	n, err := s.Backfill(context.Background())
	if err != nil {
		t.Fatalf("Backfill 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应补算 2 条，得到 %d", n)
	}

	// 补算后语义检索立刻可用
	hits, err := s.Search("数据库性能", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("补算后应能语义命中，得到 %d 条", len(hits))
	}

	// 再跑一次应为 0 条可补
	n, err = s.Backfill(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("重复补算应为 0 条，得到 %d, err=%v", n, err)
	}
}

// TestMigrateOldMemoTable 模拟没有 embedding 列的老库。
func TestMigrateOldMemoTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	oldDB, err := openRaw(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldDB.Exec(
		`INSERT INTO memo (content) VALUES ('老笔记：连接池调优')`); err != nil {
		t.Fatal(err)
	}
	oldDB.Close()

	s, err := Open(path, WithEmbedder(&fakeEmbedder{dim: 4}))
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	defer s.Close()

	// 老数据还在，且新写入的带向量
	if _, err := s.Add("新笔记：分布式缓存"); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search("分布式缓存", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("迁移后新笔记应可检索")
	}
}

// openRaw 按「迁移前」的旧结构建库（没有 embedding 列）。
func openRaw(t *testing.T, path string) (*sql.DB, error) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE memo (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		content    TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.Exec(`CREATE VIRTUAL TABLE memo_fts USING fts5(tokens, tokenize = 'unicode61')`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
