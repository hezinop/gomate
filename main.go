// todo-server 是一个独立进程的 MCP 服务器，向 Agent 暴露待办管理能力。
//
// 与 memo-server 一样走 stdio 传输：
//   - 工具是独立进程，可独立部署、独立扩缩、被任何 MCP 客户端复用
//   - stdout 是协议通道，日志一律走 stderr
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hizenop/gomate/internal/todostore"
)

// 时间解析支持的格式。模型给的时间串五花八门，多给几种容错能显著提高调用成功率。
var timeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	time.RFC3339,
}

func main() {
	dbPath := os.Getenv("GOMATE_TODO_DB")
	if dbPath == "" {
		dbPath = "./var/todo.db"
	}

	store, err := todostore.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "todo-server: 打开待办库失败: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	s := server.NewMCPServer(
		"todo-server",
		"0.1.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	s.AddTool(addTodoTool(), makeAddHandler(store))
	s.AddTool(listTodoTool(), makeListHandler(store))
	s.AddTool(doneTodoTool(), makeDoneHandler(store))
	s.AddTool(deleteTodoTool(), makeDeleteHandler(store))

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "todo-server: %v\n", err)
		os.Exit(1)
	}
}

func addTodoTool() mcp.Tool {
	return mcp.NewTool("add_todo",
		mcp.WithDescription(
			"Create a new to-do item for the user. "+
				"Use this when the user says they need to, should, or want to do something "+
				"(for example: \" remind me to ...\", \"add a task\", \"记一下明天要交报告\"). "+
				"If an identical to-do already exists, the existing one is returned instead of creating a duplicate. "+
				"Returns the created to-do with its id.",
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("The to-do title, short and specific, e.g. \"周四前提交实习简历\"."),
		),
		mcp.WithString("due_at",
			mcp.Description("Optional due time. Accepts \"2026-10-20 15:04\" or \"2026-10-20\". Leave empty if unspecified."),
		),
	)
}

func listTodoTool() mcp.Tool {
	return mcp.NewTool("list_todo",
		mcp.WithDescription(
			"List the user's to-do items. "+
				"Use this when the user asks what they need to do, wants a recap of pending tasks, "+
				"or before marking something done so you can locate it. "+
				"Returns to-dos with id, title, status and due time.",
		),
		mcp.WithString("status",
			mcp.Description("Filter by status: \"pending\" or \"done\". Omit to list all."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of items to return. Defaults to 20."),
		),
	)
}

func doneTodoTool() mcp.Tool {
	return mcp.NewTool("done_todo",
		mcp.WithDescription(
			"Mark a to-do as done. "+
				"Use this when the user says they finished something (for example: \"报告交了\", \"done with X\"). "+
				"Identify the item either by its numeric id (preferred, from list_todo) or by its exact title.",
		),
		mcp.WithNumber("id",
			mcp.Description("The to-do id, as returned by list_todo. Preferred over title."),
		),
		mcp.WithString("title",
			mcp.Description("The exact to-do title, used only when id is unknown."),
		),
	)
}

func deleteTodoTool() mcp.Tool {
	return mcp.NewTool("delete_todo",
		mcp.WithDescription(
			"Delete a to-do item. Use this only when the user explicitly asks to remove or cancel a task. "+
				"Identify it by numeric id (preferred) or exact title.",
		),
		mcp.WithNumber("id",
			mcp.Description("The to-do id, as returned by list_todo. Preferred over title."),
		),
		mcp.WithString("title",
			mcp.Description("The exact to-do title, used only when id is unknown."),
		),
	)
}

func makeAddHandler(store *todostore.Store) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, err := req.RequireString("title")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		dueAt, err := parseDue(req)
		if err != nil {
			// 时间解析失败不阻塞创建 —— 待办本身才是用户要的
			dueAt = nil
		}

		td, created, err := store.Add(title, dueAt)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("创建待办失败: %v", err)), nil
		}
		if !created {
			return mcp.NewToolResultText(fmt.Sprintf("已存在同名待办，未重复创建：%s", formatTodo(td))), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("已创建待办：%s", formatTodo(td))), nil
	}
}

func makeListHandler(store *todostore.Store) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		status, _ := req.RequireString("status")
		if status != todostore.StatusPending && status != todostore.StatusDone {
			status = "" // 无效值当作不过滤，而不是报错给模型添乱
		}
		limit := 20
		if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}

		list, err := store.List(status, limit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("查询待办失败: %v", err)), nil
		}
		if len(list) == 0 {
			return mcp.NewToolResultText("当前没有符合条件的待办。"), nil
		}

		var b strings.Builder
		b.WriteString(fmt.Sprintf("共 %d 条待办：\n", len(list)))
		for _, t := range list {
			b.WriteString("- " + formatTodo(t) + "\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

func makeDoneHandler(store *todostore.Store) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, title := pickTarget(req)
		td, err := store.MarkDone(id, title)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("标记完成失败: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("已完成：%s", formatTodo(td))), nil
	}
}

func makeDeleteHandler(store *todostore.Store) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, title := pickTarget(req)
		if err := store.Delete(id, title); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("删除失败: %v", err)), nil
		}
		return mcp.NewToolResultText("已删除。"), nil
	}
}

// pickTarget 从参数里取定位用的 id 或 title。id 优先。
func pickTarget(req mcp.CallToolRequest) (int64, string) {
	var id int64
	if v, ok := req.GetArguments()["id"].(float64); ok {
		id = int64(v)
	}
	title, _ := req.RequireString("title")
	return id, title
}

func parseDue(req mcp.CallToolRequest) (*time.Time, error) {
	raw, err := req.RequireString("due_at")
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("未指定时间")
	}
	raw = strings.TrimSpace(raw)
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("无法解析时间 %q", raw)
}

func formatTodo(t todostore.Todo) string {
	mark := "[ ]"
	if t.Status == todostore.StatusDone {
		mark = "[x]"
	}
	due := ""
	if t.DueAt != nil {
		due = fmt.Sprintf(" (截止 %s)", t.DueAt.Format("2006-01-02 15:04"))
	}
	return fmt.Sprintf("id=%d %s %s%s", t.ID, mark, t.Title, due)
}
