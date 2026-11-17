package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/mcpjungle/mcpjungle/pkg/version"
)

const (
	metaToolDescribe = "mcpjungle__describe_tool"
	metaToolUse      = "mcpjungle__use_tool"

	lazyMCPInstructions = "This is a lazy MCP gateway with two meta-tools: `mcpjungle__describe_tool` returns the input schema for a given tool name; `mcpjungle__use_tool` invokes a tool. The `mcpjungle__use_tool` description lists all available tools and shows the exact call format. Always describe before you invoke."
)

type lazyContextKey string

const lazyGroupContextKey lazyContextKey = "lazy_group"

// ToolGroupResolver is the subset of ToolGroupService used by lazy mode.
type ToolGroupResolver interface {
	ResolveEffectiveTools(name string) ([]string, error)
}

// WithLazyGroup binds a lazy request to an optional tool group.
func (m *MCPService) WithLazyGroup(ctx context.Context, group string) context.Context {
	return context.WithValue(ctx, lazyGroupContextKey, group)
}

func lazyGroupFromContext(ctx context.Context) string {
	group, _ := ctx.Value(lazyGroupContextKey).(string)
	return group
}

func newLazyMCPServer() *server.MCPServer {
	return server.NewMCPServer(
		"MCPJungle lazy MCP proxy",
		version.GetVersion(),
		server.WithToolCapabilities(true),
		server.WithInstructions(lazyMCPInstructions),
	)
}

// initializeLazyHandlers registers the two fixed lazy meta-tools and sets
// use_tool's description to the current bullet-list catalog.
func (m *MCPService) initializeLazyHandlers() {
	m.rebuildLazyToolDescriptions()
}

// rebuildLazyToolDescriptions regenerates the bullet-list catalog from the
// current set of visible enabled tools and updates the lazy server's
// use_tool description. Safe to call concurrently; uses the mcp-go
// SetTools API which is internally locked.
func (m *MCPService) rebuildLazyToolDescriptions() {
	tools, err := m.ListTools()
	if err != nil {
		log.Printf("[WARN] rebuildLazyToolDescriptions: failed to list tools: %v", err)
		return
	}

	var visible []model.Tool
	for _, tool := range tools {
		if !tool.Enabled {
			continue
		}
		parent, _, ok := splitServerToolName(tool.Name)
		if !ok {
			continue
		}
		srv, err := m.GetMcpServer(parent)
		if err != nil || !srv.Enabled {
			continue
		}
		visible = append(visible, tool)
	}

	useDesc := buildUseToolDescription(visible)

	m.lazyMcpProxyServer.SetTools(
		server.ServerTool{
			Tool: mcp.NewTool(
				metaToolDescribe,
				mcp.WithDescription(
					"Returns the full input schema (JSON Schema) for a single MCP tool. "+
						"You MUST call this before `mcpjungle__use_tool` to learn the exact arguments a tool expects. "+
						"Do not invent or guess argument names.",
				),
				mcp.WithString("tool_name", mcp.Required(),
					mcp.Description("Canonical tool name in the form server__tool.")),
			),
			Handler: m.lazyDescribeToolHandler,
		},
		server.ServerTool{
			Tool: mcp.NewTool(
				metaToolUse,
				mcp.WithDescription(useDesc),
				mcp.WithString("tool_name", mcp.Required(),
					mcp.Description("Canonical tool name in the form server__tool.")),
				mcp.WithAny("tool_args", mcp.Required(),
					mcp.Description("JSON object matching the selected tool's input schema. Obtain the schema via `mcpjungle__describe_tool` first.")),
			),
			Handler: m.lazyUseToolHandler,
		},
	)
}

// lazyDescribeToolHandler looks up a tool by canonical name and returns its
// full original entry (name, description, input schema) as JSON text.
func (m *MCPService) lazyDescribeToolHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := strings.TrimSpace(req.GetString("tool_name", ""))
	if name == "" {
		return mcp.NewToolResultError("`tool_name` is required"), nil
	}

	tool, err := m.GetTool(name)
	if err != nil || !tool.Enabled {
		return mcp.NewToolResultErrorf("MCP tool %s not found or disabled", name), nil
	}

	parent, _, ok := splitServerToolName(name)
	if !ok {
		return mcp.NewToolResultErrorf("MCP tool %s has an invalid canonical name", name), nil
	}
	srv, err := m.GetMcpServer(parent)
	if err != nil || !srv.Enabled {
		return mcp.NewToolResultErrorf("MCP server %s is unavailable", parent), nil
	}

	var schema any = map[string]any{}
	if len(tool.InputSchema) > 0 {
		_ = json.Unmarshal(tool.InputSchema, &schema)
	}

	entry := map[string]any{
		"name":         tool.Name,
		"description":  tool.Description,
		"input_schema": schema,
	}
	return lazyJSONResult(entry)
}

// lazyUseToolHandler authorizes and routes a tool invocation to the correct
// upstream server via the existing MCPProxyToolCallHandler.
func (m *MCPService) lazyUseToolHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := strings.TrimSpace(req.GetString("tool_name", ""))
	if name == "" {
		return mcp.NewToolResultError("`tool_name` is required"), nil
	}

	serverName, _, ok := splitServerToolName(name)
	if !ok {
		return mcp.NewToolResultError("`tool_name` must be a canonical `server__tool` name"), nil
	}

	if err := m.authorizeLazyServer(ctx, serverName); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !allowed[name] {
		return mcp.NewToolResultErrorf("MCP tool %s is not available", name), nil
	}

	arguments, ok := req.GetArguments()["tool_args"].(map[string]any)
	if !ok {
		return mcp.NewToolResultError("`tool_args` must be a JSON object matching the tool's input schema"), nil
	}

	result, err := m.MCPProxyToolCallHandler(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: arguments}})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// lazyAllowedTools returns the set of enabled tool canonical names the
// caller is allowed to invoke. In enterprise mode without a tool group it
// still returns every enabled tool — `authorizeProxyServerAccess` is the
// boundary that restricts which servers' tools actually surface.
func (m *MCPService) lazyAllowedTools(ctx context.Context) (map[string]bool, error) {
	if group := lazyGroupFromContext(ctx); group != "" {
		if m.toolGroupService == nil {
			return nil, fmt.Errorf("tool groups are unavailable")
		}
		tools, err := m.toolGroupService.ResolveEffectiveTools(group)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve tool group %q: %w", group, err)
		}
		allowed := make(map[string]bool, len(tools))
		for _, tool := range tools {
			allowed[tool] = true
		}
		return allowed, nil
	}
	tools, err := m.ListTools()
	if err != nil {
		return nil, fmt.Errorf("failed to list MCP tools: %w", err)
	}
	allowed := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool.Enabled {
			allowed[tool.Name] = true
		}
	}
	return allowed, nil
}

// authorizeLazyServer confirms a server exists, is enabled, is accessible to
// the caller under the enterprise allow list, and (for tool-group requests)
// contributes at least one allowed tool.
func (m *MCPService) authorizeLazyServer(ctx context.Context, serverName string) error {
	serverModel, err := m.GetMcpServer(serverName)
	if err != nil || !serverModel.Enabled {
		return fmt.Errorf("MCP server %s is unavailable", serverName)
	}
	if err := authorizeProxyServerAccess(ctx, serverName); err != nil {
		return err
	}
	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return err
	}
	for toolName := range allowed {
		parent, _, _ := splitServerToolName(toolName)
		if parent == serverName {
			return nil
		}
	}
	return fmt.Errorf("MCP server %s is unavailable in this tool group", serverName)
}

var sentenceEndRe = regexp.MustCompile(`\.(?:\r?\n| [A-Z])`)

// firstSentence extracts the first sentence from a description. It cuts at
// the first period followed by a newline, or by a space followed by an
// uppercase letter. If no such marker exists, the full description is
// returned unchanged.
func firstSentence(desc string) string {
	m := sentenceEndRe.FindStringIndex(desc)
	if m == nil {
		return desc
	}
	return desc[:m[0]+1]
}

// buildUseToolDescription renders the use_tool description: clear calling
// instructions, a concrete example, and the sorted bullet-list catalog of
// every visible enabled tool.
func buildUseToolDescription(tools []model.Tool) string {
	var sb strings.Builder

	sb.WriteString("Invoke an upstream MCP tool. For each tool in the list below, call this tool like this:\n")
	sb.WriteString("  mcpjungle__use_tool({\"tool_name\": \"<server>__<tool>\", \"tool_args\": {...}})\n")
	sb.WriteString("You MUST first call `mcpjungle__describe_tool` with the tool name to get its input schema. Do not invent or guess argument names.\n")
	sb.WriteString("Example:\n")
	sb.WriteString("  mcpjungle__use_tool({\"tool_name\": \"github__create_issue\", \"tool_args\": {\"owner\": \"me\", \"repo\": \"x\", \"title\": \"Bug: something\"}})\n")
	sb.WriteString("\nAvailable tools:\n")

	if len(tools) == 0 {
		sb.WriteString("(none currently available)\n")
		return strings.TrimRight(sb.String(), "\n")
	}

	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)

	byName := make(map[string]model.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}

	for _, name := range names {
		sb.WriteString("- ")
		sb.WriteString(name)
		sb.WriteString(": ")
		sb.WriteString(firstSentence(byName[name].Description))
		sb.WriteString("\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}

func lazyJSONResult(value any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}
