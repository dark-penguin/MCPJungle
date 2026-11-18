package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/mcpjungle/mcpjungle/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lazyTestService(t *testing.T) *MCPService {
	t.Helper()
	db := setupTestDBWithTools(t)
	service := &MCPService{
		db:                 db,
		metrics:            telemetry.NewNoopCustomMetrics(),
		lazyMcpProxyServer: newLazyMCPServer(),
	}
	service.initializeLazyHandlers()
	return service
}

func devCtx() context.Context {
	return context.WithValue(context.Background(), "mode", model.ModeDev)
}

func createLazyTestServer(t *testing.T, service *MCPService, name string) *model.McpServer {
	t.Helper()
	serverModel, err := model.NewStdioServer(name, name+" description", "echo", nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, service.db.Create(serverModel).Error)
	return serverModel
}

// createLazyTestTool creates a tool in the DB with the original (unprefixed) name.
func createLazyTestTool(t *testing.T, service *MCPService, serverModel *model.McpServer, originalName string, description string) *model.Tool {
	t.Helper()
	schema, err := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}})
	require.NoError(t, err)
	tool := &model.Tool{ServerID: serverModel.ID, Name: originalName, Description: description, InputSchema: schema, Enabled: true}
	require.NoError(t, service.db.Create(tool).Error)
	return tool
}

func lazyRequest(name string, arguments map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = arguments
	return req
}

func lazyResultJSON(t *testing.T, result *mcp.CallToolResult, destination any) {
	t.Helper()
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(text.Text), destination))
}

// --- Task 1.1: Exactly two tools exposed ---

func TestLazy_ExposeExactlyTwoMetaTools(t *testing.T) {
	service := lazyTestService(t)
	tools := service.lazyMcpProxyServer.ListTools()
	assert.Len(t, tools, 2)
	assert.Contains(t, tools, metaToolDescribe)
	assert.Contains(t, tools, metaToolUse)
}

// --- Task 1.2: describe_tool handler ---

func TestLazy_DescribeTool_KnownTool(t *testing.T) {
	service := lazyTestService(t)
	srv := createLazyTestServer(t, service, "alpha")
	createLazyTestTool(t, service, srv, "greet", "Greets a person by name. Returns a friendly message.")
	service.rebuildLazyToolDescriptions()

	result, err := service.lazyDescribeToolHandler(devCtx(), lazyRequest(metaToolDescribe, map[string]any{"tool_name": "alpha__greet"}))
	require.NoError(t, err)
	var entry map[string]any
	lazyResultJSON(t, result, &entry)
	assert.Equal(t, "alpha__greet", entry["name"])
	assert.Contains(t, entry["description"], "Greets a person")
	schema, ok := entry["input_schema"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", schema["type"])
}

func TestLazy_DescribeTool_UnknownTool(t *testing.T) {
	service := lazyTestService(t)
	result, err := service.lazyDescribeToolHandler(devCtx(), lazyRequest(metaToolDescribe, map[string]any{"tool_name": "nope__missing"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestLazy_DescribeTool_EmptyName(t *testing.T) {
	service := lazyTestService(t)
	result, err := service.lazyDescribeToolHandler(devCtx(), lazyRequest(metaToolDescribe, map[string]any{"tool_name": ""}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestLazy_DescribeTool_DisabledServer(t *testing.T) {
	service := lazyTestService(t)
	srv := createLazyTestServer(t, service, "alpha")
	createLazyTestTool(t, service, srv, "greet", "Greets someone.")
	// Disable the server
	require.NoError(t, service.db.Model(srv).Update("enabled", false).Error)

	result, err := service.lazyDescribeToolHandler(devCtx(), lazyRequest(metaToolDescribe, map[string]any{"tool_name": "alpha__greet"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestLazy_DescribeTool_UnknownServer(t *testing.T) {
	service := lazyTestService(t)
	result, err := service.lazyDescribeToolHandler(devCtx(), lazyRequest(metaToolDescribe, map[string]any{"tool_name": "ghost__tool"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// --- Task 1.3: use_tool handler ---

func TestLazy_UseTool_InvalidName(t *testing.T) {
	service := lazyTestService(t)
	result, err := service.lazyUseToolHandler(devCtx(), lazyRequest(metaToolUse, map[string]any{
		"tool_name": "no-underscore-here",
		"tool_args": map[string]any{},
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestLazy_UseTool_UnknownServer(t *testing.T) {
	service := lazyTestService(t)
	result, err := service.lazyUseToolHandler(devCtx(), lazyRequest(metaToolUse, map[string]any{
		"tool_name": "ghost__tool",
		"tool_args": map[string]any{},
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestLazy_UseTool_DisabledTool(t *testing.T) {
	service := lazyTestService(t)
	srv := createLazyTestServer(t, service, "alpha")
	tool := createLazyTestTool(t, service, srv, "greet", "Greets someone.")
	// Disable the tool
	require.NoError(t, service.db.Model(tool).Update("enabled", false).Error)

	result, err := service.lazyUseToolHandler(devCtx(), lazyRequest(metaToolUse, map[string]any{
		"tool_name": "alpha__greet",
		"tool_args": map[string]any{},
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// --- Task 2.1: firstSentence and buildUseToolDescription ---

func TestFirstSentence_Normal(t *testing.T) {
	assert.Equal(t, "Does a thing.", firstSentence("Does a thing. Then more."))
}

func TestFirstSentence_NoPeriod(t *testing.T) {
	assert.Equal(t, "No period here", firstSentence("No period here"))
}

func TestFirstSentence_Abbreviation(t *testing.T) {
	// ". b" is lowercase so skipped; ". P" matches -> cuts after "service."
	assert.Equal(t, "U.S. based service.", firstSentence("U.S. based service. Provides data."))
}

func TestFirstSentence_Newline(t *testing.T) {
	assert.Equal(t, "First line.", firstSentence("First line.\nSecond line."))
}

func TestFirstSentence_CRLFNewline(t *testing.T) {
	assert.Equal(t, "First line.", firstSentence("First line.\r\nSecond line."))
}

func TestFirstSentence_PeriodSpaceLowercase(t *testing.T) {
	// ". a" is lowercase so no match; full string returned
	assert.Equal(t, "No capital after period. all lowercase rest.", firstSentence("No capital after period. all lowercase rest."))
}

func TestFirstSentence_PeriodEndOfString(t *testing.T) {
	// Period at end of string with nothing after: no match
	assert.Equal(t, "Just ends here.", firstSentence("Just ends here."))
}

func TestBuildUseToolDescription_Empty(t *testing.T) {
	desc := buildUseToolDescription(nil)
	assert.Contains(t, desc, "(none currently available)")
}

func TestBuildUseToolDescription_Multiple(t *testing.T) {
	tools := []model.Tool{
		{Name: "b__two", Description: "Second tool. More info."},
		{Name: "a__one", Description: "First tool does things."},
	}
	desc := buildUseToolDescription(tools)
	assert.Contains(t, desc, "Available tools:")
	// Sorted alphabetically
	idxA := strings.Index(desc, "- a__one")
	idxB := strings.Index(desc, "- b__two")
	require.NotEqual(t, -1, idxA)
	require.NotEqual(t, -1, idxB)
	assert.Less(t, idxA, idxB)
	assert.Contains(t, desc, "First tool does things.")
	assert.Contains(t, desc, "Second tool.")
	// Contains instructions
	assert.Contains(t, desc, "mcpjungle__describe_tool")
	assert.Contains(t, desc, "tool_args")
}

// --- Task 2.2: Initial description contains one bullet per visible tool ---

func TestLazy_InitialDescriptionContainsAllTools(t *testing.T) {
	service := lazyTestService(t)
	srv1 := createLazyTestServer(t, service, "alpha")
	createLazyTestTool(t, service, srv1, "one", "First tool. Does stuff.")
	createLazyTestTool(t, service, srv1, "two", "Second tool.")
	srv2 := createLazyTestServer(t, service, "beta")
	createLazyTestTool(t, service, srv2, "three", "Third tool here.")

	service.rebuildLazyToolDescriptions()

	tools := service.lazyMcpProxyServer.ListTools()
	desc := tools[metaToolUse].Tool.Description
	assert.Contains(t, desc, "alpha__one")
	assert.Contains(t, desc, "alpha__two")
	assert.Contains(t, desc, "beta__three")
}

// --- Task 2.3: Rebuild updates bullet list on tool change ---

func TestLazy_RebuildReflectsToolChanges(t *testing.T) {
	service := lazyTestService(t)
	srv := createLazyTestServer(t, service, "alpha")
	tool1 := createLazyTestTool(t, service, srv, "one", "First tool.")
	createLazyTestTool(t, service, srv, "two", "Second tool.")

	service.rebuildLazyToolDescriptions()
	tools := service.lazyMcpProxyServer.ListTools()
	assert.Contains(t, tools[metaToolUse].Tool.Description, "alpha__one")
	assert.Contains(t, tools[metaToolUse].Tool.Description, "alpha__two")

	// Disable tool1 and rebuild
	require.NoError(t, service.db.Model(tool1).Update("enabled", false).Error)
	service.rebuildLazyToolDescriptions()
	tools = service.lazyMcpProxyServer.ListTools()
	assert.NotContains(t, tools[metaToolUse].Tool.Description, "alpha__one")
	assert.Contains(t, tools[metaToolUse].Tool.Description, "alpha__two")

	// Re-enable and rebuild
	require.NoError(t, service.db.Model(tool1).Update("enabled", true).Error)
	service.rebuildLazyToolDescriptions()
	tools = service.lazyMcpProxyServer.ListTools()
	assert.Contains(t, tools[metaToolUse].Tool.Description, "alpha__one")
}

// --- Ensure the lazy server instructions mention the meta-tools ---

func TestLazy_InstructionsMentionMetaTools(t *testing.T) {
	assert.Contains(t, lazyMCPInstructions, "mcpjungle__describe_tool")
	assert.Contains(t, lazyMCPInstructions, "mcpjungle__use_tool")
}
