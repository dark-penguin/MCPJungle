package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLazyMCPClient connects to an MCP endpoint with ?mode=lazy appended.
func newLazyMCPClient(t *testing.T, env *e2eEnv, path string, token string) *client.Client {
	t.Helper()
	opts := []transport.StreamableHTTPCOption{}
	if token != "" {
		opts = append(opts, transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + token,
		}))
	}
	c, err := client.NewStreamableHttpClient(env.baseURL+path+"?mode=lazy", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Initialize(context.Background(), mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "e2e-lazy-client", Version: "1.0.0"},
		},
	})
	require.NoError(t, err)
	return c
}

func listToolNames(t *testing.T, c *client.Client) []string {
	t.Helper()
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callTool(t *testing.T, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	require.NoError(t, err)
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content")
	return text.Text
}

func TestE2E_LazyMode_TwoMetaTools_DiscoverDescribeInvoke(t *testing.T) {
	env := setupE2EServer(t, model.ModeDev)
	registerEverythingServer(t, env, "")

	c := newLazyMCPClient(t, env, "/mcp", "")

	// The lazy endpoint exposes exactly two meta-tools.
	assert.ElementsMatch(t,
		[]string{"mcpjungle__describe_tool", "mcpjungle__use_tool"},
		listToolNames(t, c),
	)

	// use_tool's description contains the catalog with everything__echo.
	toolsRes, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	var useDesc string
	for _, tool := range toolsRes.Tools {
		if tool.Name == "mcpjungle__use_tool" {
			useDesc = tool.Description
		}
	}
	assert.Contains(t, useDesc, "everything__echo")

	// Describe a known tool to get its full schema.
	descRes := callTool(t, c, "mcpjungle__describe_tool", map[string]any{"tool_name": "everything__echo"})
	require.False(t, descRes.IsError, resultText(t, descRes))
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, descRes)), &entry))
	assert.Equal(t, "everything__echo", entry["name"])
	_, hasSchema := entry["input_schema"]
	assert.True(t, hasSchema)

	// Describe an unknown tool returns an error.
	unknownRes := callTool(t, c, "mcpjungle__describe_tool", map[string]any{"tool_name": "nope__missing"})
	assert.True(t, unknownRes.IsError)

	// Invoke a tool through use_tool.
	useRes := callTool(t, c, "mcpjungle__use_tool", map[string]any{
		"tool_name": "everything__echo",
		"tool_args": map[string]any{"message": "lazy hello"},
	})
	require.False(t, useRes.IsError, resultText(t, useRes))
	assert.Contains(t, resultText(t, useRes), "lazy hello")

	// The eager endpoint is unaffected by lazy-mode dispatch.
	eager := newMCPProxyClient(t, env, "")
	defer eager.Close()
	assert.Contains(t, listToolNames(t, eager), "everything__echo")
}

func TestE2E_LazyMode_EnterpriseAllowListEnforced(t *testing.T) {
	env := setupE2EServer(t, model.ModeEnterprise)
	registerEverythingServer(t, env, env.adminToken)

	deniedToken := createMcpClient(t, env, "lazy-denied", []string{"other"})
	denied := newLazyMCPClient(t, env, "/mcp", deniedToken)

	// The catalog is visible in use_tool's description (full catalog, not filtered per session).
	toolsRes, err := denied.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	var useDesc string
	for _, tool := range toolsRes.Tools {
		if tool.Name == "mcpjungle__use_tool" {
			useDesc = tool.Description
		}
	}
	assert.Contains(t, useDesc, "everything__echo")

	// But use_tool is rejected for a server the caller cannot access.
	invokeRes := callTool(t, denied, "mcpjungle__use_tool", map[string]any{
		"tool_name": "everything__echo",
		"tool_args": map[string]any{"message": "nope"},
	})
	assert.True(t, invokeRes.IsError)

	allowedToken := createMcpClient(t, env, "lazy-allowed", []string{"everything"})
	allowed := newLazyMCPClient(t, env, "/mcp", allowedToken)
	useRes := callTool(t, allowed, "mcpjungle__use_tool", map[string]any{
		"tool_name": "everything__echo",
		"tool_args": map[string]any{"message": "allowed"},
	})
	require.False(t, useRes.IsError, resultText(t, useRes))
	assert.Contains(t, resultText(t, useRes), "allowed")
}

func TestE2E_LazyMode_GroupEndpointLimitsTools(t *testing.T) {
	env := setupE2EServer(t, model.ModeDev)
	registerEverythingServer(t, env, "")

	resp := env.do(t, http.MethodPost, "/api/v0/tool-groups", map[string]any{
		"name":           "lazy-group",
		"included_tools": []string{"everything__echo"},
	}, "")
	drain(resp)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	c := newLazyMCPClient(t, env, "/v0/groups/lazy-group/mcp", "")

	// The group endpoint still exposes the two meta-tools.
	assert.ElementsMatch(t,
		[]string{"mcpjungle__describe_tool", "mcpjungle__use_tool"},
		listToolNames(t, c),
	)

	// Invoking a tool that is in the group works.
	useRes := callTool(t, c, "mcpjungle__use_tool", map[string]any{
		"tool_name": "everything__echo",
		"tool_args": map[string]any{"message": "group ok"},
	})
	require.False(t, useRes.IsError, resultText(t, useRes))
}
