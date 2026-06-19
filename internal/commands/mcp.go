package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/jordanknott/taskcafe/internal/config"
	"github.com/jordanknott/taskcafe/internal/db"
	"github.com/spf13/cobra"
)

// JSON-RPC 2.0 wire types

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP result types

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// Tool output shapes

type projectEntry struct {
	ID      string `db:"id"      json:"id"`
	Name    string `db:"name"    json:"name"`
	ShortID string `db:"short_id" json:"short_id"`
}

type taskEntry struct {
	Name      string `json:"name"`
	Complete  bool   `json:"complete"`
	DueDate   string `json:"due_date,omitempty"`
	GroupName string `json:"group_name"`
	ShortID   string `json:"short_id"`
}

type mcpHandler struct {
	sqlxDB  *sqlx.DB
	queries *db.Queries
}

func newMcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP stdio server",
		Long:  "Run a Model Context Protocol server over stdio for Claude Code integration",
		RunE: func(cmd *cobra.Command, args []string) error {
			appConfig, err := config.GetAppConfig()
			if err != nil {
				return err
			}
			sqlxDB, err := sqlx.Connect("postgres", appConfig.Database.GetDatabaseConnectionUri())
			if err != nil {
				return err
			}
			defer sqlxDB.Close()

			h := &mcpHandler{sqlxDB: sqlxDB, queries: db.New(sqlxDB.DB)}

			enc := json.NewEncoder(os.Stdout)
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Buffer(make([]byte, 1<<20), 1<<20)

			for scanner.Scan() {
				var req rpcRequest
				if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
					enc.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
					continue
				}
				h.handle(enc, &req)
			}
			return scanner.Err()
		},
	}
}

func (h *mcpHandler) handle(enc *json.Encoder, req *rpcRequest) {
	switch req.Method {
	case "initialize":
		enc.Encode(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]string{"name": "taskcafe", "version": "1.0.0"},
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			},
		})
	case "notifications/initialized":
		// notification — no response
	case "tools/list":
		enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{"tools": toolSchemas()}})
	case "tools/call":
		result, err := h.callTool(req.Params)
		if err != nil {
			enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32603, Message: err.Error()}})
			return
		}
		enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
	default:
		enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}})
	}
}

func toolSchemas() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "list_projects",
			"description": "List all Taskcafe projects (id, name, short_id)",
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name":        "get_project_tasks",
			"description": "Get all tasks for a project, grouped by task group",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"project_id": map[string]interface{}{"type": "string", "description": "UUID of the project"},
				},
				"required": []string{"project_id"},
			},
		},
	}
}

func (h *mcpHandler) callTool(params json.RawMessage) (mcpToolResult, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return mcpToolResult{}, err
	}
	ctx := context.Background()
	switch p.Name {
	case "list_projects":
		return h.listProjects(ctx)
	case "get_project_tasks":
		return h.getProjectTasks(ctx, p.Arguments)
	default:
		return mcpToolResult{}, fmt.Errorf("unknown tool: %s", p.Name)
	}
}

func (h *mcpHandler) listProjects(ctx context.Context) (mcpToolResult, error) {
	var rows []projectEntry
	err := h.sqlxDB.SelectContext(ctx, &rows, "SELECT project_id AS id, name, short_id FROM project ORDER BY name")
	if err != nil {
		return mcpToolResult{}, err
	}
	if rows == nil {
		rows = []projectEntry{}
	}
	text, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return mcpToolResult{}, err
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(text)}}}, nil
}

func (h *mcpHandler) getProjectTasks(ctx context.Context, arguments json.RawMessage) (mcpToolResult, error) {
	var args struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return mcpToolResult{}, err
	}
	projectUUID, err := uuid.Parse(args.ProjectID)
	if err != nil {
		return mcpToolResult{}, fmt.Errorf("invalid project_id: %w", err)
	}
	groups, err := h.queries.GetTaskGroupsForProject(ctx, projectUUID)
	if err != nil {
		return mcpToolResult{}, err
	}
	tasks := []taskEntry{}
	for _, group := range groups {
		groupTasks, err := h.queries.GetTasksForTaskGroupID(ctx, group.TaskGroupID)
		if err != nil {
			return mcpToolResult{}, err
		}
		for _, t := range groupTasks {
			entry := taskEntry{
				Name:      t.Name,
				Complete:  t.Complete,
				GroupName: group.Name,
				ShortID:   t.ShortID,
			}
			if t.DueDate.Valid {
				entry.DueDate = t.DueDate.Time.Format("2006-01-02")
			}
			tasks = append(tasks, entry)
		}
	}
	text, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return mcpToolResult{}, err
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(text)}}}, nil
}
