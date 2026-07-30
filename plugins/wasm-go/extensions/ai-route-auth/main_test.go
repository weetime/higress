package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// 两个授权维度是 OR 语义：allow_workspace_projects 与 allow_apikeys 只要有一个非空，
// 该路由就不是 deny all。这几个用例锁住这个语义 —— parseRuleConfig 里的告警文案必须与之一致，
// 否则「仅配 allow_apikeys」的路由会被误报成 "will deny all API keys"。
const (
	// 仅配 allow_apikeys：apikey 命中即放行，与 workspace/project 无关。
	cfgApiKeysOnly = `{
		"user_apikeys": {"admin": ["sk-admin"]},
		"_rules_": [{
			"_match_route_": ["route-apikeys-only"],
			"rule_name": "apikeys-only",
			"allow_apikeys": ["sk-admin"]
		}]
	}`

	// 两个维度都为空：这才是真正的 deny all。
	cfgBothEmpty = `{
		"user_apikeys": {"admin": ["sk-admin"]},
		"_rules_": [{
			"_match_route_": ["route-both-empty"],
			"rule_name": "both-empty"
		}]
	}`

	// 仅配 allow_workspace_projects：原有的项目维度鉴权路径。
	cfgWorkspaceOnly = `{
		"workspace_users": {"ws-a": ["admin"]},
		"user_apikeys": {"admin": ["sk-admin"]},
		"_rules_": [{
			"_match_route_": ["route-workspace-only"],
			"rule_name": "workspace-only",
			"allow_workspace_projects": ["ws-a:*"]
		}]
	}`
)

func authHeaders() [][2]string {
	return [][2]string{
		{":authority", "example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
		{"authorization", "Bearer sk-admin"},
	}
}

// 仅配 allow_apikeys 的路由必须放行 —— 证明 "allow_workspace_projects 为空 => deny all" 是伪命题。
func TestApiKeysOnlyRouteIsAllowed(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgApiKeysOnly))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-apikeys-only"))

		require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders(authHeaders()))
		require.Nil(t, host.GetLocalResponse(), "只配 allow_apikeys 的路由不应被拒绝")
	})
}

// 仅配 allow_workspace_projects 的路由必须放行（向后兼容）。
func TestWorkspaceOnlyRouteIsAllowed(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgWorkspaceOnly))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-workspace-only"))

		require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders(authHeaders()))
		require.Nil(t, host.GetLocalResponse(), "只配 allow_workspace_projects 的路由不应被拒绝")
	})
}

// 两个维度都为空才 deny all。
func TestBothEmptyRouteIsDenied(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgBothEmpty))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-both-empty"))

		host.CallOnHttpRequestHeaders(authHeaders())
		resp := host.GetLocalResponse()
		require.NotNil(t, resp, "两个 allow 列表都为空时该路由必须拒绝")
		require.Equal(t, uint32(403), resp.StatusCode)
	})
}

// 未命中任何 matchRule 的路由跳过鉴权、放行。
func TestUnmatchedRouteIsSkipped(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgApiKeysOnly))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("some-other-route"))

		require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders(authHeaders()))
		require.Nil(t, host.GetLocalResponse(), "未命中 matchRule 的路由应放行")
	})
}
