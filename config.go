// Package config 集中管理所有运行时配置。
//
// 配置优先级（高 -> 低）：
//
//  1. 进程环境变量（shell export / docker -e / K8s ConfigMap）
//  2. 项目根目录的 .env 文件
//  3. 代码内默认值
//
// 这样本地开发时一个 .env 文件就能搞定，生产容器里又能完全不依赖文件，
// 直接用环境变量把某个键顶掉。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config 是 gomate 的全部运行时配置。
type Config struct {
	// LLM 相关。BaseURL 允许指向任何 OpenAI 兼容端点，
	// 因此 DeepSeek / 通义 / 智谱 / 本地 Ollama 都能直接切换。
	LLMAPIKey  string
	LLMBaseURL string
	LLMModel   string

	// Memo MCP Server 相关。这两个字段装载的一定是绝对路径 ——
	// Load 会把相对路径按 ProjectRoot 展开。因此无论从哪个目录启动
	// （项目根、bin/、或任何别的地方）都指向同一份数据。
	MemoDBPath  string // SQLite 文件位置
	MemoBinPath string // memo-server 可执行文件（Agent 通过 stdio 拉起它）

	// 另外三个 MCP Server（待办 / 文件检索 / 提醒）
	TodoDBPath        string
	TodoBinPath       string
	FileSearchBinPath string
	SearchRoot        string // 文件检索允许访问的根目录（沙箱边界）
	ReminderDBPath    string
	ReminderBinPath   string

	SummaryDBPath string // 长期摘要库

	ChatDBPath string // 聊天会话库（会话持久化）

	// 语义检索（④）。BaseURL/Model 有默认值（SiliconFlow + bge-m3），
	// APIKey 为空表示不启用——检索自动退化为纯 FTS 关键词。
	EmbeddingAPIKey  string
	EmbeddingBaseURL string
	EmbeddingModel   string

	// HTTP 服务
	HTTPAddr string

	// 运行时治理（W3）
	MaxSteps       int           // Agent 最大步数，防模型死循环
	RequestTimeout time.Duration // 单请求总超时
	MemoryBudget   int           // 短期记忆 token 预算
	TokenBudget    int           // 单会话累计 token 上限，成本控制

	// AgentTaskTimeout 是定时 Agent 任务（③）单次执行的超时。
	// 比 RequestTimeout 更宽松：任务可能在无人值守时跑（如凌晨），
	// 且失败只影响这一次推送，不影响交互式请求。
	AgentTaskTimeout time.Duration

	// ProjectRoot 是定位到 .env 的那个目录，用作相对路径的基准。
	// 没找到 .env 时退化成当前工作目录。
	ProjectRoot string

	// sources 记录每个键最终取自哪里，仅供 String() / 诊断输出使用。
	sources map[string]Source
}

const (
	dotEnvName = ".env"

	// maxUpwardLevels 向上查找 .env 的最大层数。
	// 目的是让 `cd bin && ./gomate` 这类用法也能找到项目根的 .env。
	maxUpwardLevels = 6
)

// Source 描述某个配置项最终取自哪里，仅用于诊断输出。
type Source string

const (
	SourceEnv      Source = "环境变量"
	SourceDotEnv   Source = ".env"
	SourceDefault  Source = "默认值"
	SourceNotFound Source = "未设置"
)

// Load 按「环境变量 > .env > 默认值」的优先级加载配置，并对必填项做校验。
func Load() (*Config, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("获取当前工作目录失败: %w", err)
	}

	// 注意这里用 godotenv.Read 而不是 godotenv.Load：
	// Read 只把文件解析成 map，不写进程环境变量。这样优先级完全由
	// 下面的 pick 掌控，也不会把密钥污染给后续拉起的子进程。
	envFile, root, fileEnv, err := readDotEnv(wd)
	if err != nil {
		return nil, err
	}
	if root == "" {
		root = wd
	}

	// pick 返回 (最终值, 来源)。来源信息只服务于出错时的诊断信息。
	pick := func(key, def string) (string, Source) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, SourceEnv
		}
		if len(fileEnv) > 0 {
			if v, ok := fileEnv[key]; ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v), SourceDotEnv
			}
		}
		return def, SourceDefault
	}

	// pickInt / pickDur 与 pick 同源，只是多了类型转换。
	// 解析失败时静默回退默认值 —— 配置写错不该让整个程序起不来。
	pickInt := func(key string, def int) int {
		v, _ := pick(key, "")
		if v == "" {
			return def
		}
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
			return def
		}
		return n
	}
	pickDur := func(key string, def time.Duration) time.Duration {
		v, _ := pick(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return def
		}
		return d
	}

	// p 是 pick 的简写版：只取值，不关心来源。
	p := func(key, def string) string {
		v, _ := pick(key, def)
		return v
	}

	// bin 返回某个 MCP server 可执行文件的绝对路径，并自动补 Windows 的 .exe。
	bin := func(name, envKey string) string {
		n := name
		if runtime.GOOS == "windows" {
			n += ".exe"
		}
		return absBinPath(root, p(envKey, filepath.Join("bin", n)))
	}

	key, keySrc := pick("LLM_API_KEY", "")
	baseURL, baseSrc := pick("LLM_BASE_URL", "https://api.deepseek.com")
	model, modelSrc := pick("LLM_MODEL", "deepseek-chat")
	dbPath, dbSrc := pick("GOMATE_MEMO_DB", "var/memo.db")
	// binSrc 只用于 --check 的诊断输出，取值交给 bin() 统一处理
	_, binSrc := pick("GOMATE_MEMO_BIN", "")

	c := &Config{
		LLMAPIKey:  key,
		LLMBaseURL: baseURL,
		LLMModel:   model,

		MemoDBPath:  absPath(root, dbPath),
		MemoBinPath: bin("memo-server", "GOMATE_MEMO_BIN"),

		TodoDBPath:        absPath(root, p("GOMATE_TODO_DB", "var/todo.db")),
		TodoBinPath:       bin("todo-server", "GOMATE_TODO_BIN"),
		FileSearchBinPath: bin("filesearch-server", "GOMATE_FILESEARCH_BIN"),
		SearchRoot:        absPath(root, p("GOMATE_SEARCH_ROOT", ".")),
		ReminderDBPath:    absPath(root, p("GOMATE_REMINDER_DB", "var/reminder.db")),
		ReminderBinPath:   bin("reminder-server", "GOMATE_REMINDER_BIN"),

		SummaryDBPath: absPath(root, p("GOMATE_SUMMARY_DB", "var/summary.db")),

		ChatDBPath: absPath(root, p("GOMATE_CHAT_DB", "var/chat.db")),

		EmbeddingAPIKey:  p("GOMATE_EMBEDDING_API_KEY", ""),
		EmbeddingBaseURL: p("GOMATE_EMBEDDING_BASE_URL", "https://api.siliconflow.cn/v1"),
		EmbeddingModel:   p("GOMATE_EMBEDDING_MODEL", "BAAI/bge-m3"),

		HTTPAddr: p("GOMATE_HTTP_ADDR", ":8080"),

		MaxSteps:       pickInt("GOMATE_MAX_STEPS", 12),
		RequestTimeout: pickDur("GOMATE_REQUEST_TIMEOUT", 120*time.Second),
		MemoryBudget:   pickInt("GOMATE_MEMORY_BUDGET", 3000),
		TokenBudget:    pickInt("GOMATE_TOKEN_BUDGET", 200000),

		AgentTaskTimeout: pickDur("GOMATE_AGENT_TASK_TIMEOUT", 90*time.Second),

		ProjectRoot: root,
	}

	if c.LLMAPIKey == "" {
		return nil, missingKeyDiagnostic("LLM_API_KEY", keySrc, wd, envFile, fileEnv)
	}

	// 记录每个键的来源，供 --check 的诊断输出使用。
	c.sources = map[string]Source{
		"LLM_API_KEY":     keySrc,
		"LLM_BASE_URL":    baseSrc,
		"LLM_MODEL":       modelSrc,
		"GOMATE_MEMO_DB":  dbSrc,
		"GOMATE_MEMO_BIN": binSrc,
	}
	return c, nil
}

// sources 记录每个键的实际来源，只在 String() 诊断输出里使用。

// String 打印一份安全的配置快照（密钥脱敏），供排查用。
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "项目根目录    : %s\n", c.ProjectRoot)
	fmt.Fprintf(&b, "LLM_BASE_URL  : %-46s (%s)\n", c.LLMBaseURL, c.sourceOf("LLM_BASE_URL"))
	fmt.Fprintf(&b, "LLM_MODEL     : %-46s (%s)\n", c.LLMModel, c.sourceOf("LLM_MODEL"))
	fmt.Fprintf(&b, "LLM_API_KEY   : %-46s (%s)\n", Mask(c.LLMAPIKey), c.sourceOf("LLM_API_KEY"))
	fmt.Fprintf(&b, "GOMATE_MEMO_DB : %-45s (%s)\n", c.MemoDBPath, c.sourceOf("GOMATE_MEMO_DB"))
	fmt.Fprintf(&b, "GOMATE_MEMO_BIN: %-45s (%s)\n", c.MemoBinPath, c.sourceOf("GOMATE_MEMO_BIN"))
	fmt.Fprintf(&b, "待办库/进程    : %s\n", c.TodoBinPath)
	fmt.Fprintf(&b, "文件检索进程   : %-45s\n", c.FileSearchBinPath)
	fmt.Fprintf(&b, "检索根目录     : %-45s\n", c.SearchRoot)
	fmt.Fprintf(&b, "提醒进程       : %-45s\n", c.ReminderBinPath)
	fmt.Fprintf(&b, "会话库         : %-45s\n", c.ChatDBPath)
	if c.EmbeddingAPIKey != "" {
		fmt.Fprintf(&b, "语义检索       : %-45s (%s)\n",
			c.EmbeddingModel, c.sourceOf("GOMATE_EMBEDDING_BASE_URL"))
		fmt.Fprintf(&b, "embedding Key : %-45s\n", Mask(c.EmbeddingAPIKey))
	} else {
		fmt.Fprintf(&b, "语义检索       : 未启用（未配 GOMATE_EMBEDDING_API_KEY，纯关键词检索）\n")
	}
	fmt.Fprintf(&b, "HTTP 监听      : %-45s\n", c.HTTPAddr)
	fmt.Fprintf(&b, "运行时治理     : maxSteps=%d 超时=%v 记忆预算=%d token预算=%d 任务超时=%v\n",
		c.MaxSteps, c.RequestTimeout, c.MemoryBudget, c.TokenBudget, c.AgentTaskTimeout)
	return b.String()
}

func (c *Config) sourceOf(key string) Source {
	if s, ok := c.sources[key]; ok {
		return s
	}
	return SourceDefault
}

// Mask 把敏感串脱敏成「前4后4」的形式，用于安全打印。
func Mask(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(空)"
	}
	r := []rune(s)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", 8) + string(r[len(r)-4:])
}

// readDotEnv 从 dir 逐级向上查找 .env，返回（文件路径、所在目录、解析结果）。
// 找不到时不返回错误 —— .env 本来就是可选的，生产环境完全可以只用环境变量。
func readDotEnv(dir string) (string, string, map[string]string, error) {
	prev := ""
	for i := 0; i < maxUpwardLevels; i++ {
		if dir == prev {
			break // filepath.Dir 不再变化，说明已到文件系统根
		}
		candidate := filepath.Join(dir, dotEnvName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			m, err := godotenv.Read(candidate)
			if err != nil {
				return "", "", nil, fmt.Errorf(
					"解析 %s 失败：%w\n请检查是否有语法错误（例如值里含未转义的空格或 #，需要用引号包起来）",
					candidate, err)
			}
			return candidate, dir, m, nil
		}
		prev = dir
		dir = filepath.Dir(dir)
	}
	return "", "", nil, nil
}

// missingKeyDiagnostic 把「Key 读不到」这个高频问题变成一份自查报告，
// 而不是一句让人二次困惑的 「未设置」。
func missingKeyDiagnostic(key string, src Source, wd, envFile string, fileEnv map[string]string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "未读取到 %s。已检查以下来源：\n\n", key)
	fmt.Fprintf(&b, "  1. 进程环境变量 %s -> %s\n", key, mark(src == SourceEnv))

	if envFile != "" {
		_, hit := fileEnv[key]
		fmt.Fprintf(&b, "  2. .env 文件 %s\n     -> %s", envFile, mark(hit))
		if !hit {
			ks := sortedKeys(fileEnv)
			if len(ks) == 0 {
				b.WriteString("（文件为空或只有注释）")
			} else {
				b.WriteString("\n     该文件里实际存在的键：" + strings.Join(ks, ", "))
			}
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "  2. .env 文件 -> 未找到（已从 %s 向上查找 %d 层）\n", wd, maxUpwardLevels)
	}

	b.WriteString("\n修复方式（任选其一）：\n\n")
	fmt.Fprintf(&b, "  a) 在项目根目录的 .env 里加一行：%s=你的密钥\n", key)
	fmt.Fprintf(&b, "  b) 临时导出环境变量：export %s=你的密钥\n", key)
	return errors.New(b.String())
}

func mark(ok bool) string {
	if ok {
		return "命中"
	}
	return "未命中"
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// absPath 把相对路径按 root 展开成绝对路径。
func absPath(root, p string) string {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

// absBinPath 在 absPath 之上兼容 Windows 的 .exe 后缀：
// build.sh 产出的文件名带 .exe，而 .env 里通常习惯写成不带后缀的名字。
func absBinPath(root, p string) string {
	p = absPath(root, p)
	if p == "" {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(p), ".exe") {
		candidate := p + ".exe"
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return p
}
