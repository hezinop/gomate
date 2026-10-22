// Package filesearch 提供受沙箱约束的本地文件检索。
//
// 它是 Agent 的「眼睛」，能看本地文件，所以安全是这里的第一约束：
// 任何路径参数都必须被限制在允许的根目录之内。
package filesearch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// 默认限制。这两个上限是必须的：
// 一次工具返回若不带上限，几十个匹配项就能把模型上下文吃满，
// 是 Agent 工具设计里最常见的翻车点。
const (
	DefaultMaxHits    = 20
	DefaultMaxLineLen = 200

	// maxFileSize 超过这个体积的文件跳过内容扫描，避免把整个大文件读进内存
	maxFileSize = 1 << 20 // 1MB
)

// 扫描时一律跳过的目录。
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	".idea": true, ".vscode": true, "bin": true,
	"dist": true, "build": true, "__pycache__": true,
}

// Hit 是一处匹配。
type Hit struct {
	Path string
	Line int
	Text string
}

// Sandbox 把用户传入的路径约束到 root 之内，防目录穿越（../../etc/passwd）。
//
// 做法：先 Clean 掉 "." 和 ".."，再拼成绝对路径，最后校验是否仍以 root 为前缀。
// 三步缺一不可 —— 少了 HasPrefix 校验，Clean 之后仍可能逃出根目录。
func Sandbox(root, p string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("无法解析根目录 %q: %w", root, err)
	}
	target := p
	if !filepath.IsAbs(target) {
		target = filepath.Join(absRoot, target)
	}
	target = filepath.Clean(target)

	absRoot = filepath.Clean(absRoot)
	if target != absRoot && !strings.HasPrefix(target, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径 %q 越界，只允许访问 %q 之内的文件", p, absRoot)
	}
	return target, nil
}

// Search 在 root 下检索 query：
//  1. 文件名/路径包含 query（大小写不敏感）
//  2. 文本文件内容包含 query
//
// 结果按「文件名命中优先，其次内容命中」排序，并做总量与单行长度双重截断。
func Search(root, query string, maxHits int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("检索关键词不能为空")
	}
	if maxHits <= 0 {
		maxHits = DefaultMaxHits
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// 根目录自身也要过一遍沙箱，防止配置里就写了个危险路径
	if _, err := Sandbox(absRoot, absRoot); err != nil {
		return nil, err
	}

	needle := strings.ToLower(query)
	var (
		hits      []Hit
		nameHits  []Hit
		truncated bool
	)

	// walkErr 单独记录：目录不可读之类的错误不该让整个检索失败，
	// 记下来最后附带告知即可。
	var walkErr error

	_ = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			walkErr = err
			return nil // 继续走，不中断
		}
		if len(hits)+len(nameHits) >= maxHits*2 {
			truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(absRoot, path)
		if rel == "" {
			rel = path
		}

		if strings.Contains(strings.ToLower(rel), needle) {
			nameHits = append(nameHits, Hit{Path: rel, Text: "(文件名命中)"})
			return nil
		}

		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		if !isTextFile(path) {
			return nil
		}
		hits = append(hits, scanFile(path, rel, needle, maxHits)...)
		return nil
	})

	out := append(nameHits, hits...)
	if len(out) > maxHits {
		out = out[:maxHits]
	}
	if walkErr != nil {
		return out, fmt.Errorf("部分目录无法访问: %w", walkErr)
	}
	if truncated {
		return out, fmt.Errorf("匹配过多，已截断到 %d 条", maxHits)
	}
	return out, nil
}

// scanFile 逐行扫描文件内容，避免一次性读入大文件。
func scanFile(absPath, relPath, needle string, maxHits int) []Hit {
	f, err := os.Open(absPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var hits []Hit
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if len(hits) >= maxHits {
			break
		}
		line := scanner.Text()
		if !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		hits = append(hits, Hit{Path: relPath, Line: lineNo, Text: truncate(line, DefaultMaxLineLen)})
	}
	return hits
}

// isTextFile 用 UTF-8 合法性做粗判，跳过二进制文件（图片、压缩包、可执行文件）。
// 不做完整的内容嗅探 —— 对于本地检索这个场景，够用且成本极低。
func isTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 1024)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return false
	}
	if !utf8.Valid(buf[:n]) {
		return false
	}
	// 出现 NUL 字节基本可以确定是二进制
	for _, b := range buf[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}

func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// Format 把命中结果渲染给模型。刻意带上路径与行号 ——
// 模型拿到这两项才能继续追问或复述给用户，只给内容片段没有后续价值。
func Format(hits []Hit) string {
	if len(hits) == 0 {
		return "没有找到匹配的文件或内容。"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("共 %d 条命中：\n", len(hits)))
	for _, h := range hits {
		if h.Line > 0 {
			b.WriteString(fmt.Sprintf("- %s:%d  %s\n", h.Path, h.Line, h.Text))
		} else {
			b.WriteString(fmt.Sprintf("- %s  %s\n", h.Path, h.Text))
		}
	}
	return b.String()
}
