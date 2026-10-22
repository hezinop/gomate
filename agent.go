// Package agent 封装了一个基于 Eino ReAct 的个人助理。
//
// 整个链路：
//
//	用户提问 -> ReAct Agent(ChatModel) -> 决定是否调工具
//	                                  -> ToolsNode（并发调用 MCP 工具）
//	                                  -> 结果回填给模型 -> 继续思考或直接回答
//
// 这里所有「难的部分」都由 Eino 处理，其中最值得说的是：
// 工具节点本身不支持流式，却被夹在流式链路中间 —— Eino 会自动做
// 流的 Collect/Transform 转换，无需手写 glue 代码。
package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	openai "github.com/cloudwego/eino-ext/components/model/openai"
	einomcp "github.com/cloudwego/eino-ext/components/tool/mcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hizenop/gomate/internal/config"
)

// toolTimeout 是单个工具的独立超时。
// 必须有：MCP 工具是跨进程调用，子进程挂死时若没有独立超时，
// 整个请求会一直吊着，用户永远等不到结果。
const toolTimeout = 30 * time.Second

// systemPrompt 定义助理的人格与工具使用纪律。
const systemPrompt = `你是一个懂中文的个人效率助理，名字叫 GoMate。

你可以使用以下四类工具：

1. **笔记**（search_memo / add_memo / recent_memo）—— 用户的个人笔记库
2. **待办**（add_todo / list_todo / done_todo / delete_todo）—— 待办清单
3. **本地文件检索**（search_files）—— 在用户允许的根目录内查找文件与内容
4. **提醒**（add_reminder / list_reminder / cancel_reminder）—— 定时提醒，到点会主动推送给用户

使用原则：

1. 当用户问的事情可能记过笔记时，**必须先调用 search_memo 检索**，不要凭空回答。
2. 只有当用户明确要求「记一下」「保存」「remember」时，才调用 add_memo。
   不要自作主张保存用户没有要求保存的内容。
3. 用户说要做什么事、需要做什么 → add_todo；说做完了 → done_todo；问有什么要做 → list_todo。
4. 用户说「提醒我」「X 分钟后/明天 X 点叫我」→ add_reminder。时间支持 "+10m" 这种相对写法。
5. 用户找文件、找代码 → search_files。只能用用户允许的根目录，不要尝试越界路径。
6. 一次拿不到结果就换个关键词再试；实在搜不到，就直接说明没找到，并给出你尝试过的方式。
7. 回答请简洁、直接，用中文。除非用户要求，不要长篇大论。`

// serverSpec 描述一个要拉起的 MCP server。
type serverSpec struct {
	name string // 仅用于日志与报错
	bin  string // 可执行文件路径
	env  []string
}

// Assistant 是 Agent 的门面，对外屏蔽 Eino / MCP 的细节。
type Assistant struct {
	agent     *react.Agent
	clients   []*client.Client
	toolNames []string
}

// New 构造助理：拉起全部 MCP 工具进程 -> 取工具 -> 建 ReAct Agent。
//
// tracer 用于把每一步记成 span；传 nil 则不做埋点。
func New(ctx context.Context, cfg *config.Config, mcpEnv func(...string) []string) (*Assistant, error) {
	specs := []serverSpec{
		// memo 多传三个 embedding 变量（④）：子进程据此决定是否启用语义检索。
		// 注意 .env 里的值不会自动进入子进程环境，必须由这里显式转发。
		{name: "memo", bin: cfg.MemoBinPath, env: mcpEnv(
			"GOMATE_MEMO_DB="+cfg.MemoDBPath,
			"GOMATE_EMBEDDING_API_KEY="+cfg.EmbeddingAPIKey,
			"GOMATE_EMBEDDING_BASE_URL="+cfg.EmbeddingBaseURL,
			"GOMATE_EMBEDDING_MODEL="+cfg.EmbeddingModel,
		)},
		{name: "todo", bin: cfg.TodoBinPath, env: mcpEnv("GOMATE_TODO_DB=" + cfg.TodoDBPath)},
		{name: "filesearch", bin: cfg.FileSearchBinPath, env: mcpEnv("GOMATE_SEARCH_ROOT=" + cfg.SearchRoot)},
		{name: "reminder", bin: cfg.ReminderBinPath, env: mcpEnv("GOMATE_REMINDER_DB=" + cfg.ReminderDBPath)},
	}

	var (
		tools   []tool.BaseTool
		clients []*client.Client
		failed  []string
	)

	for _, spec := range specs {
		cli, err := client.NewStdioMCPClient(spec.bin, spec.env)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: 启动失败(%v)", spec.name, err))
			continue
		}

		// MCP 协议要求：任何请求前必须先 Initialize
		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "gomate", Version: "0.1.0"}
		if _, err := cli.Initialize(ctx, initReq); err != nil {
			failed = append(failed, fmt.Sprintf("%s: 握手失败(%v)", spec.name, err))
			cli.Close()
			continue
		}

		// ToolNameList 留空 = 拉取该 server 的全部工具
		ts, err := einomcp.GetTools(ctx, &einomcp.Config{Cli: cli})
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: 取工具失败(%v)", spec.name, err))
			cli.Close()
			continue
		}

		clients = append(clients, cli)
		for _, t := range ts {
			// 每个工具套一层独立超时 —— 这是生产级 Runtime 的四件事之一
			tools = append(tools, withTimeout(t, toolTimeout))
		}
	}

	if len(tools) == 0 {
		// 一个工具都没有 = Agent 没有任何能力，直接失败比默默降级更诚实
		for _, c := range clients {
			c.Close()
		}
		return nil, fmt.Errorf("没有任何 MCP 工具可用；失败原因：%v。"+
			"请先执行 ./build.sh 构建各 server，并用 gomate --check 核对路径", failed)
	}

	// 部分失败要告知用户：静默降级会让人误以为工具坏了
	for _, f := range failed {
		fmt.Fprintf(os.Stderr, "agent: 工具 server 不可用 -> %s（其余功能不受影响）\n", f)
	}

	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if info, err := t.Info(ctx); err == nil && info != nil {
			names = append(names, info.Name)
		}
	}

	// ---- ChatModel。BaseURL 可指向任意 OpenAI 兼容端点 ----
	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.LLMAPIKey,
		BaseURL: cfg.LLMBaseURL,
		Model:   cfg.LLMModel,
		Timeout: 90 * time.Second, // 模型侧延迟不可控，超时必须显式设置
	})
	if err != nil {
		closeAll(clients)
		return nil, fmt.Errorf("构造 ChatModel 失败: %w", err)
	}

	// ---- ReAct Agent ----
	a, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: cm,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
			// ExecuteSequentially 默认 false —— 意味着一次返回多个 tool_call 时
			// Eino 会并发执行。这是 Go 版相对 Python 版的天然优势：
			// 工具的 I/O 等待可以完全重叠，不必挨个排队。
			ExecuteSequentially: false,
		},
		// Eino 不内置记忆，记忆策略必须由业务自己决定（见 internal/memory）。
		MessageModifier: func(ctx context.Context, input []*schema.Message) []*schema.Message {
			out := make([]*schema.Message, 0, len(input)+1)
			out = append(out, schema.SystemMessage(systemPrompt))
			out = append(out, input...)
			return out
		},
		// 步数上限是防止模型陷入「反复调工具」死循环的最后一道保险。
		// 没有它，一次失控的会话可能无限烧 token。
		MaxStep: cfg.MaxSteps,
	})
	if err != nil {
		closeAll(clients)
		return nil, fmt.Errorf("构造 ReAct Agent 失败: %w", err)
	}

	return &Assistant{agent: a, clients: clients, toolNames: names}, nil
}

// Generate 一次性拿到完整回复。
func (as *Assistant) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	return as.agent.Generate(ctx, input)
}

// Stream 以流式返回，用于实现打字机效果。返回的 StreamReader 用完必须 Close。
func (as *Assistant) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return as.agent.Stream(ctx, input)
}

// ToolNames 返回已接入的工具名，供 UI 展示。
func (as *Assistant) ToolNames() []string {
	out := make([]string, len(as.toolNames))
	copy(out, as.toolNames)
	return out
}

// Close 关闭所有 stdio 连接，回收 MCP 子进程。
func (as *Assistant) Close() error {
	closeAll(as.clients)
	return nil
}

func closeAll(clients []*client.Client) {
	for _, c := range clients {
		if c != nil {
			c.Close()
		}
	}
}

// ---------------------------------------------------------------- 工具超时包装

// timedTool 给任意 InvokableTool 套一层独立超时。
//
// 为什么必须包这一层：ToolsNode 并发执行工具时，任何一个工具挂住
// 都会让整条流水线卡死。有了它，超时后该工具返回错误，
// 模型能看到失败原因并换一种方式重试 —— 这正是「错误作为数据返回」的价值。
type timedTool struct {
	inner   tool.InvokableTool
	timeout time.Duration
}

// withTimeout 包装工具。若底层工具不是 InvokableTool（理论上不会），原样返回。
func withTimeout(t tool.BaseTool, d time.Duration) tool.BaseTool {
	it, ok := t.(tool.InvokableTool)
	if !ok {
		return t
	}
	return &timedTool{inner: it, timeout: d}
}

func (t *timedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *timedTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)

	// 工具实现未必遵守 ctx 取消（尤其跨进程调用），
	// 因此在 goroutine 里跑并设兜底超时 —— 双保险，保证一定不会吊死。
	go func() {
		out, err := t.inner.InvokableRun(ctx, args, opts...)
		ch <- result{out, err}
	}()

	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("工具调用超时（%v），已放弃本次调用", t.timeout)
	}
}
