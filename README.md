[README.md](https://github.com/user-attachments/files/33825019/README.md)
# GoMate — 用 Go 从零搭的个人 AI 效率助理

> 一个会调工具的个人助理：**你说人话，它去翻你的笔记、管你的待办、到点主动提醒你**。
> 核心价值不在于"又一个 ChatGPT 壳"，而在于把 **2026 年 AI 生产栈的关键环节**跑通一遍：
> Agent 编排（Eino）→ MCP 工具协议 → 流式输出 → 记忆与主动推送 → 生产级运行时 → 性能对比。

**一句话技术定位**：这是「Python 原型 → Go 生产」这条路径的个人可复现版本。
同一个 Agent 有 Go 和 Python（LangChain）两版实现，用压测数据说话，而不是用形容词。

---

## 一、当前进度：W1–W4 全部完成

| 周次 | 模块 | 位置 | 状态 |
|---|---|---|---|
| W1 | ReAct Agent（Eino 编排） | `internal/agent/` | ✅ 并发工具调用、步数上限、流式 |
| W1 | `memo-server` MCP 工具 | `cmd/memo-server/` | ✅ 笔记检索/新增/最近，SQLite FTS5 中文分词 |
| W2 | 另外三个 MCP 工具 | `cmd/todo-server/`<br>`cmd/filesearch-server/`<br>`cmd/reminder-server/` | ✅ 待办（同名幂等）<br>✅ 文件检索（路径沙箱 + 结果分块）<br>✅ 提醒（持久化，重启不丢） |
| W2 | HTTP 接入层 | `internal/server/` | ✅ SSE 流式 + WebSocket 推送 + 内嵌网页 |
| W2 | 网页 UI | `internal/server/web/` | ✅ 原生实现，打字机效果，零前端依赖 |
| W3 | 双层记忆 | `internal/memory/` | ✅ 短期按 token 预算裁剪 + 长期 LLM 摘要持久化 |
| W3 | 主动推送 | `internal/push/` | ✅ WebSocket + 单调 seq + 断线补发 + 心跳 |
| W3 | 生产级 Runtime | `internal/agent/`、`internal/server/` | ✅ maxSteps / 单工具超时 / 总超时 / token 预算 / 全链路 span |
| W4 | 压测工具 | `bench/` | ✅ SSE 并发压测，含 TTFT 与 P50/P99 |
| W4 | Python 对照实现 | `python-ref/agent.py` | ✅ LangChain 版，共用同一批 MCP server |
| W5 | 会话持久化 | `internal/chatsession/` | ✅ 聊天记录落库，重启恢复，多会话切换 |
| W5 | 执行过程可视化 | `internal/server/steps.go` | ✅ 工具调用步骤实时渲染成时间线 |
| W5 | 主动式 Agent | `internal/server/agent_task.go` | ✅ 到点自动跑一轮 Agent 并推送结果 |
| W5 | 语义记忆检索 | `internal/embedding/`、`internal/memostore/` | ✅ FTS5 + 向量混合检索（RRF），可插拔可降级 |

---

## 二、架构

```
┌───────────────────────────────────────────────────────────┐
│  网页 UI（内嵌，零依赖）                                      │
│  SSE 流式响应（打字机）  +  WebSocket 主动推送（提醒到点）      │
└────────────────────────┬──────────────────────────────────┘
                         │ POST /api/chat (SSE)   GET /api/ws
┌────────────────────────▼──────────────────────────────────┐
│  接入层 internal/server                                     │
│  会话管理 · token 预算 · 总超时 · 提醒调度循环                 │
├───────────────────────────────────────────────────────────┤
│  Agent Runtime — Eino ReAct                                │
│  ChatModel ──► 是否调工具？──► ToolsNode（并发）──► 回填再思考   │
│  maxSteps 收敛 · 单工具超时 · 全链路 span                     │
├───────────────────────────────────────────────────────────┤
│  工具层 — MCP（四个独立进程，stdio JSON-RPC）                  │
│  memo-server · todo-server · filesearch-server              │
│  reminder-server                                            │
├───────────────────────────────────────────────────────────┤
│  记忆层  短期：token 预算滑动窗口                              │
│         长期：LLM 压缩摘要 + SQLite 持久化                     │
├───────────────────────────────────────────────────────────┤
│  存储层  SQLite ×4（笔记 / 待办 / 提醒 / 摘要）                │
│  可观测  internal/telemetry：span 树 + token 计量             │
└───────────────────────────────────────────────────────────┘
```

**为什么工具是独立进程而不是进程内函数？**
MCP 是协议层，意味着工具可以独立部署、独立扩缩、被任何支持 MCP 的客户端复用
（Claude Desktop、Cursor、以及你的 Agent）。换 Agent 框架甚至换语言，工具一行不用改。

---

## 三、快速开始

### 0. 环境要求

- Go **1.25.5+**（`mcp-go v1.3.0` 的硬性要求，Go 会自动切换 toolchain）
- 一个 OpenAI 兼容的 LLM Key（推荐 DeepSeek，便宜且支持 function calling）

### 1. 构建

```bash
./build.sh          # 构建 4 个 MCP server + gomate
```

### 2. 配置 Key

```bash
cp .env.example .env
# 编辑 .env，把 LLM_API_KEY 填上即可。不需要 source。
```

程序启动时会**自动**从当前目录向上查找最近的一个 `.env` 并加载。

加载优先级（高 → 低）：

| 优先级 | 来源 | 典型场景 |
| --- | --- | --- |
| 1 | 进程环境变量 | 容器部署 `docker -e LLM_API_KEY=xxx`，临时顶掉文件配置 |
| 2 | `.env` 文件 | 本地开发 |
| 3 | 代码默认值 | `LLM_BASE_URL` 之类有合理默认值的项 |

不确定配置是否读到？一条命令自查：

```bash
./bin/gomate --check
```

会打印每个键的**实际值和来源**（密钥自动脱敏）。

### 3. 启动 Web 服务（推荐）

```bash
./bin/gomate serve                 # 默认 :8080
./bin/gomate serve -addr :8099     # 换端口
```

打开 `http://localhost:8080`，直接开聊。
提醒到点时会**主动**从右上角推给你 —— 试试说「30 秒后提醒我喝水」。

### 4. 桌面 App 模式（关掉窗口也能收提醒）

Web 版有个绕不过去的死穴：**浏览器一关，主动推送就收不到了**，
而主动提醒恰恰是这个项目最核心的能力。桌面版就是为解决这个问题。

```bash
./bin/gomated                        # 常驻系统托盘，自动打开独立窗口
./bin/gomated autostart on           # 开机自启
./bin/gomated autostart status       # 查看状态
./bin/gomated autostart off          # 取消
```

它和 `serve` 模式用的是**同一套后端**（见 `internal/app`），差别只在五条：

| | Web 版 `gomate serve` | 桌面版 `gomated` |
|---|---|---|
| 提醒触达 | 仅 WebSocket（页面必须开着） | WebSocket + **系统通知** |
| 启动方式 | 手动敲命令 | 托盘常驻，可**开机自启** |
| 界面 | 浏览器标签页 | **原生窗口**（自己的进程，无地址栏） |
| 端口冲突 | 报错退出 | **自动顺延**到空闲端口 |
| 重复启动 | 再起一份 | **单实例**：第二个实例直接退出并提示 |

**关于「原生窗口」**：用 WebView2 自己创建窗口（`internal/desktop/webview_windows.go`），
不启动 Edge/Chrome。任务栏上是 GoMate 自己的图标，进程列表里只有 `gomated.exe`，
WebView2 渲染进程挂在它下面——这是「独立应用」和「用浏览器打开网页」的本质区别。

> 为什么不用 Wails：本机 gcc 6.3.0 太老，且 Wails 要把前后端通信改成私有的
> binding/事件机制，现有的流式输出、seq 补发全部要重写。这里用纯 Go 的
> WebView2 绑定（`github.com/jchv/go-webview2`，无 CGO，WebView2Loader.dll
> 从内存加载），窗口里加载的就是同一个本地 HTTP 服务，**前后端一行都不用改**。
>
> 唯一的外部依赖是 **WebView2 运行时**：Windows 11 自带；Windows 10 只要装过
> Edge 就有（evergreen 运行时随 Edge 更新）。万一检测不到，程序会自动回退到
> Edge `--app=` 窗口模式，并弹通知说明，不会白屏死给你看。

**关闭按钮 = 隐藏到托盘**，不是退出：窗口关了服务照跑、提醒照推（窗口过程被
子类化拦截，见 `webview_windows.go` 文件头注释）。想真正退出请用托盘菜单的「退出」。

> 桌面版以 GUI 子系统构建（`-H windowsgui`），**没有控制台**。
> 日志在 `var/gomated.log`，`autostart` 命令的结果通过系统通知反馈。

### 5. 命令行模式

```bash
./bin/gomate --once "记一下：今天我们讨论了 Eino 的流式转换"
./bin/gomate --once "我之前关于流式记了什么"    # 触发 search_memo
./bin/gomate                                  # 交互模式
```

### 6. 不配 Key 也能验证存储层

```bash
go run ./cmd/memoctl add "今天我们讨论了分布式缓存的一致性哈希方案"
go run ./cmd/memoctl search "分布式缓存"
go run ./cmd/memoctl tokenize "分布式缓存 Redis"   # 看中文分词效果
```

---

### 7. W5 新能力：会话、时间线、定时任务、语义检索

**会话持久化**：左侧边栏列出全部会话（标题取首条消息），点击切换、可删除；
刷新页面或重启应用都回到原会话，Agent 上下文接着历史走（不只是渲染历史）。
当前会话 id 存在 localStorage。

**执行过程可视化**：提问后每条助手回复下方会实时渲染执行时间线——
每一步工具调用（名称、参数、结果、耗时），可展开详情。这就是 ReAct 循环的
「思考 → 调工具 → 再思考」过程，不再是黑盒。数据经 Eino 回调实时推送
（`internal/server/steps.go`），并随消息落库。

**主动式 Agent**：提醒分两种，对 Agent 说「10 分钟后提醒我喝水」是普通提醒；
说「明早 9 点总结我的待办和昨天的笔记，生成一份晨报推送给我」则是**定时
Agent 任务**——到点自动跑一轮带完整工具的 Agent，把产出结果通过页面卡片 +
系统通知推给你。实现要点：reminder 表扩展 kind/prompt/result 四态，
ClaimDue 事务防重原样复用，任务异步执行 + 信号量限流 + 独立超时。

**语义记忆检索**：配了 embedding key（见 .env.example）后，笔记检索从纯关键词
升级为「FTS5 + 向量 cosine」混合，两路排名用 RRF 融合——搜「数据库性能」能找到
通篇没有这几个字的「连接池调优」笔记。没配 key 自动降级纯关键词，功能照常。
首次启用后跑一次 `go run ./cmd/memoctl embed` 给老笔记补算向量。

> 为什么不上向量数据库：个人笔记量级（千条以内）暴力全量 cosine 是毫秒级。
> 数据规模决定架构——这个取舍本身比「用了什么」更值得讲。

## 四、关键设计决策（面试时能讲的）

### 4.1 工具节点夹在流式链路里，流为什么不断？

这是 Agent 流式比普通 LLM 流式复杂的地方 —— 工具节点本身不支持流，插在中间会断。

Eino 的处理是**自动做流的转换**：上游的 token 流被 Collect 成完整输入喂给工具节点，
工具输出再 Transform 回流继续往下传。在 LangChain 里这段 glue 要手写，且极易写错。

### 4.2 一次返回多个工具调用怎么办？并发。

`compose.ToolsNodeConfig.ExecuteSequentially` 默认就是 `false`，
Eino 会并发执行多个 tool_call，I/O 等待完全重叠。这是 Go 版相对 Python 版的天然优势。

### 4.3 怎么防止模型陷入死循环烧钱？

四层防护，不是一层：

1. **maxSteps**（默认 12）硬上限，超出强制收敛
2. **单个工具独立超时** 30s（`internal/agent` 的 `timedTool`）—— 跨进程调用挂死时尤其关键
3. **单请求总超时**（默认 120s），并可被 Ctrl+C 取消
4. **会话级 token 预算**，超了直接拒绝不再烧钱

### 4.4 记忆为什么按 token 预算裁剪，而不是按轮数？

按轮数是错的：一轮闲聊和一轮塞进整篇文档，长度能差两个数量级。
`internal/memory` 按估算 token 做滑动窗口，并且**只在问答对边界切**
（不把"用户提问"和"助手回答"切开，否则模型会看到残缺上下文）。

被挤出窗口的历史不直接丢弃 —— 用一次额外 LLM 调用压缩成摘要持久化，
后续对话注入回 system prompt，实现跨会话还记得。压缩失败也不阻断对话。

### 4.5 主动推送怎么保证不丢消息？

复用的是和 IM 会话 seq 同一套思路（`internal/push`）：

- 每条消���带**单调递增 seq**
- 服务端保留最近 128 条的环形缓冲
- 客户端断线重连时带 `?last_seq=N`，服务端补发缺失部分
- 心跳保活，慢客户端丢弃而非阻塞广播（否则一个死连接会拖住提醒调度）

提醒本身也存在 SQLite 里，**领取和改状态在同一事务内完成**，
所以进程重启、多实例都不会重复推。

### 4.6 安全：Agent 能读文件 = 能读机器上的任何东西

`internal/filesearch.Sandbox` 做三步校验：`Clean` → 拼绝对路径 → `HasPrefix` 校验仍在根目录内。
三步缺一不可 —— 少了 HasPrefix，`Clean` 之后仍可能逃出去。
工具报错一律**作为数据返回**而非抛异常，这样模型能看到失败原因并自我纠正。

### 4.7 可观测：为什么自己写 span 而不直接上 OTel SDK

Eino 官方生态**没有**现成的 OTel callbacks adapter（已核实不存在），
而它对外暴露的标准接入点就是 `callbacks.Handler`。
本项目实现了这个接口（`internal/telemetry`），把「提问 → 思考 → 调工具 → 回答」
每一步记成 span，在 `/api/trace?session=xxx` 直接看得到。

将来要接真 OTel，只需替换 exporter，业务代码零改动。

> 注意：只注册了非流式三个回调时机。流式回调会收到一份 stream 拷贝且**必须由 handler 关闭**，
> 漏关会导致整条流水线 goroutine 与内存泄漏。流式调用的 token 用量改从
> `ResponseMeta.Usage` 读取后回填。

### 4.8 工具为什么调不起来？大概率是描述写得太短

工具 `Description` 是模型决定是否调用它的**唯一依据**，本质是写给模型看的系统提示。
第一版写「读取文件」，模型几乎从不调用；改成说清「什么时候用、返回什么」之后就正常了。

### 4.9 中文为什么能用 SQLite FTS5 搜得到？

FTS5 的 `unicode61` 分词器按空格和标点切词，而中文没有词边界，
不处理的话整句会被当成一个 token，检索彻底失效。
`internal/memostore.Tokenize` 对汉字做**二元切分**（"分布式缓存" → "分布 布式 式缓 缓存"），
以少量精度换取接近完整的召回。

### 4.10 stdio 传输的隐藏地雷

MCP server 走 stdio 时，**stdout 就是 JSON-RPC 通道**，
任何往 stdout 打的日志都会破坏报文。所有日志必须走 stderr。

### 4.11 工具调用可视化：为什么走回调，而不是解构流式 chunk

react agent 的中间轮工具调用流被图分支的检查器消费掉了，调用方从
StreamReader 里只能看到最终答案——想「看到」工具调用，解构 chunk 这条路
根本不存在。正解是 Eino 的回调系统：每个工具执行都经 ReuseHandlers 带上
自己的 RunInfo 触发 OnStart/OnEnd（`compose/tool_node.go`），
耗时用 OnStart 往 ctx 里塞时间戳、OnEnd 取出来算，与 telemetry 传 span id
是同一个技巧。回调里拿不到 SSE writer，就用 ctx 把 emitter 带下去。

### 4.12 定时 Agent 任务：复用调度，还是新写一套

reminder 表加 kind/prompt/result 三个列，ClaimDue 的事务防重、1 秒轮询、
onFire 注入全部原样复用，onFire 里按 kind 分支——agent 任务异步跑
`Assistant.Generate`（不占调度循环），信号量限流（到点任务可能扎堆），
独立超时（无人值守时跑，比交互式请求宽松）。「已领取但没跑完」的崩溃窗口
与普通提醒取舍一致：接受丢失，不接受重复。

### 4.13 混合检索为什么用 RRF，以及为什么不上向量数据库

FTS5 的 rank 与 cosine 量纲不同，直接加权要先归一化再调权重；
RRF 只用排名（1/(k+rank)），免去这一切。数据量决定架构：
个人笔记千条以内，暴力全量 cosine 是毫秒级，引 FAISS/Milvus 属杀鸡用牛刀，
还多一个要运维的组件。embedding 客户端可插拔、无 key 自动降级纯关键词——
项目对没有 key 的人必须仍然可用，降级是设计的一部分而不是意外。

### 4.14 一个 emoji 让系统通知整条消失（Windows toast 的坑）

go-toast 用 CDATA 拼 toast XML，**星位平面字符（超出 BMP 的 emoji，
如 🤖）会让 XML 加载直接失败**，报一个完全看不出原因的 doc.LoadXml 错误；
BMP 内的 ⏰ 和中文却都正常。通知是「关掉窗口也能收提醒」的关键通道，
不能因为标题里带了个新 emoji 就静默消失——`desktop.Notify` 统一兜底替换。
这类「渲染链路上游不支持某个 Unicode 区段」的坑，值得当成一类问题记住。

---

## 五、Go vs Python 对比压测

压测工具：

```bash
./bin/gomate serve &                       # 起服务
go run ./bench -n 100 -c 20                # 100 请求 / 20 并发
```

指标说明：

- **TTFT（首 token 延迟）**：主要由模型侧决定，语言/框架影响很小 ——
  **预期 Go 与 Python 差别不大**。如实报告这一点，比硬吹 Go 更快有价值得多。
- **总延迟**：才体现框架与运行时差异（流拼接、工具并发、GC、连接复用）。

| 指标 | Python + LangChain | Go + Eino | 说明 |
| --- | --- | --- | --- |
| 内存基线（空闲） | 待实测 | 待实测 | 预期量级 ~250MB vs ~25MB |
| 冷启动时间 | 待实测 | 待实测 | 预期量级 ~2s vs ~50ms |
| 流式 TTFT p50 | 待实测 | 待实测 | **预计接近**，瓶颈在模型 API |
| 100 并发总延迟 P99 | 待实测 | 待实测 | 这一项才体现运行时差异 |
| 部署产物体积 | 待实测 | 待实测 | 单二进制 vs 镜像 + 依赖 |
| 单请求 token 成本 | 相同 | 相同 | 证明「重写不影响效果」 |

> ⚠️ 数字必须自己跑，不要照抄预期量级。
> 跑出来 Go 版某些指标不占优**很正常，反而是加分项** ——
> 你要能解释「为什么这里没差别」（瓶颈在模型 API 而非语言）。

Python 对照版见 `python-ref/agent.py`，它复用**同一批 MCP server**，
所以工具层完全一致，压测对比才有意义。

---

## 六、已知约束 / 待办

- **单人部署**：没有用户体系与鉴权，适合本地自用
- **记忆是单机的**：会话状态在服务进程内存里，重启会丢（长期摘要已持久化，不受影响）
- **无向量检索**：跨会话语义检索属可选加分项，MVP 未做
- **token 估算为启发式**：按字符比例估算，未接真实 tokenizer

---

## 七、排障

**页面右上角显示「推送断开，重连中…」**

这不是 WebSocket 有 bug，是**服务进程没在跑**。页面是之前加载的，
HTTP 通的时候 WS 就一定通（两者是同一个进程、同一个端口）。

```bash
tasklist /FI "IMAGENAME eq gomate.exe"    # 先确认进程在不在
./bin/gomate serve                        # 不在就起它，页面会自动重连
```

判定方法：连得上页面但推送断开 → 几乎必然是进程被停了（Ctrl+C、关终端、
或被 taskkill）。实测同一条连接稳定维持 75 秒以上、每 20 秒一次心跳，无异常断开。

**提醒设了但网页没收到**

- 提醒是**持久化在 SQLite** 里的，由 `serve` 进程的调度器每秒轮询触发。
  所以 `--once` 命令行设的提醒，只要 serve 在跑，网页照样收得到。
- 反过来，**serve 没跑就没有任何推送** —— 命令行模式自己不带调度循环。
- 服务重启会清空内存环形缓冲，且已触发的提醒已标记 `fired` 不会重推。
  这是当前的设计取舍（缓冲不做持久化），断线期间漏掉的提醒需要靠
  客户端重连带 `last_seq` 补发，且仅在服务未重启时有效。

**8080 端口起不来**

```
listen tcp :8080: bind: address already in use
```

换一个端口即可：`./bin/gomate serve -addr :8097`。
（`GOMATE_HTTP_ADDR` 也可以写进 `.env`。）

顺带一提，桌面版 `gomated` 会**自动顺延**到空闲端口，不会报这个错。

**桌面版没反应（没窗口、没托盘）**

它是 GUI 程序、没有控制台，所以一切看日志：

```bash
cat var/gomated.log
```

常见原因：

- 端口被占 → 日志会打印「端口 X 被占用，自动改用 Y」，属正常行为
- 日志出现「未检测到 WebView2 运行时，改用浏览器窗口模式」→ 系统缺 WebView2
  运行时，装一下微软的 [Evergreen Bootstrapper](https://developer.microsoft.com/microsoft-edge/webview2/)
  （约 1.5 MB）即可恢复原生窗口；当前会先用 Edge `--app=` 顶着
- 日志出现「原生窗口模式」但窗口迟迟不出现 → 启动初期窗口显示的是
  「GoMate 正在启动…」占位页，等服务就绪（日志出现 `server: 监听`）才切过去，
  最多等 20 秒；超时日志会写「服务启动超时」
- 通知没弹出来 → Windows 的「专注助手 / 免打扰」会把通知折叠进通知中心。
  日志里没有「系统通知失败」就说明程序侧是成功的

**开机自启没生效**

用 `gomated autostart status` 看注册的命令。若你当时是用 `go run` 执行的，
程序会警告并拒绝写入——临时二进制会被系统清理，注册了也没用，
必须先 `./build.sh` 再用 `bin/gomated.exe` 执行。

---

## 八、测试

```bash
go test ./...
```

已覆盖：中文分词检索、路径沙箱（含目录穿越用例）、待办幂等、
提醒领取不重复、记忆裁剪与摘要持久化、WebSocket seq 补发、配置加载优先级。

---

## 九、目录结构

```
cmd/gomate/              Agent 入口：CLI 对话 + Web 服务
cmd/gomated/             桌面版入口：原生窗口 + 托盘常驻 + 系统通知 + 开机自启 + 单实例
cmd/memo-server/         MCP · 笔记
cmd/todo-server/         MCP · 待办
cmd/filesearch-server/   MCP · 本地文件检索
cmd/reminder-server/     MCP · 定时提醒
cmd/memoctl/             本地调试工具（不配 Key 也能用）
internal/agent/          Eino 编排 + 工具超时包装
internal/server/         HTTP 接入层 + SSE + 内嵌前端 + step 事件 + 定时任务执行
internal/chatsession/    聊天会话持久化（①）
internal/embedding/      OpenAI 兼容 embedding 客户端（④，可插拔）
internal/memory/         双层记忆
internal/push/           WebSocket 推送（seq + 补发）
internal/reminder/       提醒存储 + 调度器（含定时 Agent 任务）
internal/telemetry/      span 树 + token 计量
internal/app/            装配层：把各模块组装成可运行实例（两个入口共用）
internal/desktop/        桌面能力：原生窗口(WebView2) / 系统通知 / 开机自启 / 单实例
internal/{memo,todo}store/、filesearch/   存储与检索（memo 为 FTS+向量混合）
python-ref/              LangChain 对照实现
bench/                   SSE 压测工具
```

## 参考

- [Eino 文档](https://www.cloudwego.io/zh/docs/eino/)
- [MCP 协议](https://modelcontextprotocol.io/)
- [mcp-go](https://github.com/mark3labs/mcp-go)
