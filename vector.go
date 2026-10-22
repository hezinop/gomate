package memostore

// 向量存储与混合检索融合（④ 语义检索）。
//
// 存储格式：float32 小端字节流（BLOB）。选它而不是 JSON/文本：
// 1024 维向量 JSON 要几十 KB，二进制只要 4 KB，且免去解析开销。

import (
	"encoding/binary"
	"math"
	"sort"
)

// rrfK 是 RRF（Reciprocal Rank Fusion）的平滑常数，业界惯例 60。
//
// 为什么用 RRF 而不是加权分：两路分数量纲完全不同
// （FTS5 的 rank 是负数相关度，cosine 是 [-1,1]），直接加权要先归一化，
// 而权重又得靠调。RRF 只用排名 1/(k+rank)，免去这一切，
// 是混合检索的标准做法。
const rrfK = 60

// rrfFuse 融合两路排序结果，取融合分最高的 limit 条。
func rrfFuse(a, b []Memo, limit int) []Memo {
	scores := make(map[int64]float64)
	memos := make(map[int64]Memo)
	for rank, m := range a {
		scores[m.ID] += 1.0 / (rrfK + float64(rank+1))
		memos[m.ID] = m
	}
	for rank, m := range b {
		scores[m.ID] += 1.0 / (rrfK + float64(rank+1))
		memos[m.ID] = m
	}

	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // 同分按 id，保证结果确定（测试友好）
	})

	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]Memo, 0, len(ids))
	for _, id := range ids {
		out = append(out, memos[id])
	}
	return out
}

// cosine 计算两个向量的余弦相似度。任一为零向量返回 0。
func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64 // 用 float64 累加，避免长向量精度损失
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// encodeVec 把 float32 向量编码为小端字节流。
func encodeVec(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// decodeVec 把小端字节流解码为 float32 向量。长度不整 return nil。
func decodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
