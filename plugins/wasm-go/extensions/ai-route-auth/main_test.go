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

	// AI fallback 的 internal_redirect 重入场景：主路由与 fallback 路由都对 sk-admin 放行。
	// 重入那一趟 Authorization 已被上一趟的 ai-proxy 换成上游 provider 的 apiToken。
	cfgFallbackRoutes = `{
		"user_apikeys": {"admin": ["sk-admin"]},
		"_rules_": [{
			"_match_route_": ["route-primary", "route-primary-fallback"],
			"rule_name": "primary",
			"allow_apikeys": ["sk-admin"]
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

// fallbackHeaders 构造 AI fallback internal_redirect 重入那一趟的请求头：
// Authorization 已被上一趟的 ai-proxy 替换成上游 provider 的 apiToken，
// 用户的原始凭证由 ai-proxy 保存在 X-HI-ORIGINAL-AUTH 中。
func fallbackHeaders() [][2]string {
	return [][2]string{
		{":authority", "example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
		{"authorization", "Bearer sk-infer-upstream-provider-token"},
		{"x-hi-original-auth", "Bearer sk-admin"},
		{"x-higress-fallback-from", "route-primary"},
	}
}

// fallback 重试必须复用用户的原始凭证。否则读到的是上游 apiToken，
// 在 user_apikeys 里查不到，主模型明明有权限的调用会在降级时 403。
func TestFallbackReentryUsesOriginalAuth(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgFallbackRoutes))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-primary-fallback"))

		require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders(fallbackHeaders()))
		require.Nil(t, host.GetLocalResponse(), "fallback 重试不应因 Authorization 已被改写而被拒绝")
	})
}

// 重入时解析出的身份必须还是原始用户，而不是上游 provider token —— 下游按
// x-mse-consumer 限流/计费的插件依赖这个值。
func TestFallbackReentryKeepsOriginalConsumer(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgFallbackRoutes))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-primary-fallback"))

		host.CallOnHttpRequestHeaders(fallbackHeaders())
		got := map[string]string{}
		for _, h := range host.GetRequestHeaders() {
			got[h[0]] = h[1]
		}
		require.Equal(t, "admin/sk-admin", got["x-mse-consumer"])
	})
}

// 首跳（无 x-higress-fallback-from）必须忽略客户端伪造的 X-HI-ORIGINAL-AUTH，
// 只认 Authorization —— 该 header 只有在网关自己的 internal_redirect 重入时才可信。
func TestFirstHopIgnoresSpoofedOriginalAuth(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgFallbackRoutes))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-primary"))

		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"authorization", "Bearer sk-not-a-user-key"},
			{"x-hi-original-auth", "Bearer sk-admin"},
		})
		resp := host.GetLocalResponse()
		require.NotNil(t, resp, "首跳不得信任伪造的 X-HI-ORIGINAL-AUTH")
		require.Equal(t, uint32(403), resp.StatusCode)
	})
}

// 重入时若 X-HI-ORIGINAL-AUTH 缺失（例如上游未经 ai-proxy 改写 Authorization），
// 回落到 Authorization，保持与首跳一致的行为。
func TestFallbackReentryFallsBackToAuthorization(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(json.RawMessage(cfgFallbackRoutes))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("route-primary-fallback"))

		require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"authorization", "Bearer sk-admin"},
			{"x-higress-fallback-from", "route-primary"},
		}))
		require.Nil(t, host.GetLocalResponse(), "无 X-HI-ORIGINAL-AUTH 时应回落到 Authorization")
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
