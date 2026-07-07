// Copyright (c) 2023 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 测试配置
// ============================================================================

// 全局字典：两个用户/apikey、两个组、两个工具集。
var globalDicts = map[string]interface{}{
	"keys": []string{"Authorization", "x-api-key"},
	"user_apikeys": map[string]interface{}{
		"admin": []string{"sk-aaa"},
		"alice": []string{"sk-ccc"},
	},
	"group_users": map[string]interface{}{
		"ops":     []string{"admin"},
		"readers": []string{"admin", "alice"},
	},
	"tool_groups": map[string]interface{}{
		"read-tools":  []string{"get_weather", "search"},
		"admin-tools": []string{"delete_resource"},
	},
}

// matchRoute 是路由级规则匹配的路由名；请求测试需 host.SetRouteName(matchRoute)。
const matchRoute = "mcp-route"

// ruleConfig 构造「全局字典 + 一条路由级规则」的配置。
// MCP Filter 框架仅在 override 解析路径（命中 _rules_）才安装请求/响应处理器，
// 因此路由级字段必须放在 _rules_ 内，否则插件按「未绑定」放行、不解析 body。
func ruleConfig(routeFields map[string]interface{}) json.RawMessage {
	rule := map[string]interface{}{
		"_match_route_": []string{matchRoute},
	}
	for k, v := range routeFields {
		rule[k] = v
	}
	m := make(map[string]interface{}, len(globalDicts)+1)
	for k, v := range globalDicts {
		m[k] = v
	}
	m["_rules_"] = []map[string]interface{}{rule}
	data, _ := json.Marshal(m)
	return data
}

// globalOnlyConfig 只有全局字典、无 _rules_：路由未绑定，应放行。
func globalOnlyConfig() json.RawMessage {
	data, _ := json.Marshal(globalDicts)
	return data
}

// readersGrantConfig：readers 组 -> read-tools；ops 组 -> 全部工具；apikey sk-zzz 直授 read-tools。
var readersGrantConfig = ruleConfig(map[string]interface{}{
	"grants": []map[string]interface{}{
		{"group": "readers", "tool_groups": []string{"read-tools"}},
		{"group": "ops", "tool_groups": []string{"*"}},
		{"apikey": "sk-zzz", "tool_groups": []string{"read-tools"}},
	},
})

// opsOnlyGrantConfig：仅授权 ops 组（用于测 alice 无权访问）。
var opsOnlyGrantConfig = ruleConfig(map[string]interface{}{
	"grants": []map[string]interface{}{
		{"group": "ops", "tool_groups": []string{"*"}},
	},
})

// noGrantConfig：只有全局字典，没有 _rules_（路由未绑定，应放行）。
var noGrantConfig = globalOnlyConfig()

// legacyAllowConfig：旧 allow 列表（向后兼容）。
var legacyAllowConfig = ruleConfig(map[string]interface{}{
	"allow": []string{"ak-123"},
})

// 无效配置
var missingKeysConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"user_apikeys": map[string]interface{}{"admin": []string{"sk-aaa"}},
	})
	return data
}()

var emptyKeysConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"keys": []string{},
	})
	return data
}()

// grant 缺少主体（既无 group 也无 apikey）：应跳过该 grant，不报错。
var grantWithoutSubjectConfig = ruleConfig(map[string]interface{}{
	"grants": []map[string]interface{}{
		{"tool_groups": []string{"read-tools"}},
	},
})

// discoveryGrantConfig：只读发现凭证——list_only 授全部工具集，
// 可见全量 tools/list，但不能调用任何工具。直授 apikey，无需 user_apikeys 映射。
var discoveryGrantConfig = ruleConfig(map[string]interface{}{
	"grants": []map[string]interface{}{
		{"apikey": "sk-mcp-tools-discovery", "tool_groups": []string{"*"}, "list_only": true},
	},
})

// ============================================================================
// JSON-RPC 请求/响应体
// ============================================================================

const toolsListRequest = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

func toolsCallRequest(name string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`)
}

// tools/list 后端响应：含 read-tools 与 admin-tools 全部三个工具。
const toolsListResponse = `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
	`{"name":"get_weather","description":"w"},` +
	`{"name":"search","description":"s"},` +
	`{"name":"delete_resource","description":"d"}` +
	`]}}`

// jsonHeaders 构造 POST + application/json 请求头。
func jsonRequestHeaders(extra ...[2]string) [][2]string {
	h := [][2]string{
		{":authority", "mcp.example.com"},
		{":method", "POST"},
		{":path", "/mcp"},
		{"content-type", "application/json"},
	}
	return append(h, extra...)
}

func jsonResponseHeaders() [][2]string {
	return [][2]string{
		{":status", "200"},
		{"content-type", "application/json"},
	}
}

// ============================================================================
// 配置解析
// ============================================================================

func TestParseConfig(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		t.Run("valid full config", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
			config, err := host.GetMatchConfig()
			require.NoError(t, err)
			require.NotNil(t, config)
		})

		// 韧性：缺/空 keys 不应导致插件启动失败（回退 Authorization）。
		// 配置解析绝不能 fail —— 否则 fail-closed 会拖垮整条过滤器链（整网关 NFCF）。
		t.Run("missing keys -> still ok (fallback)", func(t *testing.T) {
			host, status := test.NewTestHost(missingKeysConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
		})

		t.Run("empty keys -> still ok (fallback)", func(t *testing.T) {
			host, status := test.NewTestHost(emptyKeysConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
		})

		t.Run("grant without subject -> ok (skipped)", func(t *testing.T) {
			host, status := test.NewTestHost(grantWithoutSubjectConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
		})
	})
}

// TestParseOverrideNilGlobal 回归：globalConfig 为 nil（defaultConfigDisable=true 时）
// 不能返回 error —— 否则 OnPluginStart 失败、fail-closed、整网关 500 NFCF。
func TestParseOverrideNilGlobal(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		// 启动一个 host 让 proxywasm 日志宿主就绪（parseOverrideRuleConfig 内部会写日志）。
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		ruleBytes, _ := json.Marshal(map[string]interface{}{
			"grants": []map[string]interface{}{
				{"group": "readers", "tool_groups": []string{"read-tools"}},
			},
		})

		var out any
		err := parseOverrideRuleConfig(ruleBytes, nil, &out)
		require.NoError(t, err)

		cfg, ok := out.(McpAuthConfig)
		require.True(t, ok)
		require.NotNil(t, cfg.apiKeyToUser)
		require.NotNil(t, cfg.userToGroups)
		require.NotNil(t, cfg.toolGroups)
		require.Len(t, cfg.grants, 1)
	})
}

// TestParseOverrideFullRuleConfig 验证「字典全部写在规则里 + global 为 nil」
// （即 defaultConfigDisable=true 的生产场景）：规则自带 keys/user_apikeys/group_users/
// tool_groups/grants，全部应从规则字节解析出来，无需依赖继承自 global 的字典。
func TestParseOverrideFullRuleConfig(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		// 启动 host 让日志宿主就绪。
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		ruleBytes, _ := json.Marshal(map[string]interface{}{
			"keys": []string{"Authorization"},
			"user_apikeys": map[string]interface{}{
				"admin": []string{"sk-aaa", "ak-bbb"},
				"alice": []string{"sk-ccc"},
			},
			"group_users": map[string]interface{}{
				"ops":     []string{"admin"},
				"readers": []string{"admin", "alice"},
			},
			"tool_groups": map[string]interface{}{
				"read-tools": []string{"get_weather", "search"},
			},
			"grants": []map[string]interface{}{
				{"group": "ops", "tool_groups": []string{"*"}},
				{"apikey": "sk-ccc", "tool_groups": []string{"read-tools"}},
			},
		})

		var out any
		err := parseOverrideRuleConfig(ruleBytes, nil, &out)
		require.NoError(t, err)

		cfg, ok := out.(McpAuthConfig)
		require.True(t, ok)
		require.Equal(t, []string{"Authorization"}, cfg.Keys)
		// user_apikeys 反转为 apikey -> user
		require.Equal(t, "admin", cfg.apiKeyToUser["sk-aaa"])
		require.Equal(t, "admin", cfg.apiKeyToUser["ak-bbb"])
		require.Equal(t, "alice", cfg.apiKeyToUser["sk-ccc"])
		// group_users 反转为 user -> [group]
		require.Contains(t, cfg.userToGroups["admin"], "ops")
		require.Contains(t, cfg.userToGroups["admin"], "readers")
		require.Equal(t, []string{"get_weather", "search"}, cfg.toolGroups["read-tools"])
		require.Len(t, cfg.grants, 2)
	})
}

// fullRuleOnlyConfig 构造「顶层无任何字典、字典与授权全部在 _rules_ 内」的配置，
// 模拟生产 yaml（defaultConfigDisable=true，字典挪进 matchRules）。
func fullRuleOnlyConfig() json.RawMessage {
	rule := map[string]interface{}{
		"_match_route_": []string{matchRoute},
		"keys":          []string{"Authorization"},
		"user_apikeys": map[string]interface{}{
			"admin": []string{"sk-aaa"},
			"alice": []string{"sk-ccc"},
		},
		"group_users": map[string]interface{}{
			"ops":     []string{"admin"},
			"readers": []string{"admin", "alice"},
		},
		"tool_groups": map[string]interface{}{
			"read-tools": []string{"get_weather", "search"},
		},
		"grants": []map[string]interface{}{
			{"group": "ops", "tool_groups": []string{"*"}},
		},
	}
	data, _ := json.Marshal(map[string]interface{}{
		"_rules_": []map[string]interface{}{rule},
	})
	return data
}

// TestFullRuleOnlyEndToEnd 端到端：字典只写在 _rules_ 内（顶层无字典），
// ops 组成员 admin（sk-aaa）调用工具应放行（不再 401 No Key）。
func TestFullRuleOnlyEndToEnd(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(fullRuleOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.InitHttp()
		host.SetRouteName(matchRoute)
		// 头阶段会 Pause 以缓冲 body，这里不对 action 断言。
		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnHttpRequestBody(toolsCallRequest("get_weather"))

		// 放行：无本地拒绝响应。
		require.Nil(t, host.GetLocalResponse())
	})
}

// ============================================================================
// 阶段一：身份门禁
// ============================================================================

func TestServerAccessGate(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("no token -> 401", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders())
			host.CallOnHttpRequestBody([]byte(toolsListRequest))

			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.EqualValues(t, 401, resp.StatusCode)
			require.Contains(t, string(resp.Data), "No Key")
			host.CompleteHttp()
		})

		t.Run("multi token -> 401", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			// Authorization 与 x-api-key 都带值 -> 多 token
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-ccc"},
				[2]string{"x-api-key", "sk-aaa"},
			))
			host.CallOnHttpRequestBody([]byte(toolsListRequest))

			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.EqualValues(t, 401, resp.StatusCode)
			require.Contains(t, string(resp.Data), "Multi Key")
			host.CompleteHttp()
		})

		t.Run("unknown apikey / no matching grant -> 403", func(t *testing.T) {
			// alice 属于 readers，但本路由只授 ops -> 不命中
			host, status := test.NewTestHost(opsOnlyGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-ccc"},
			))
			host.CallOnHttpRequestBody([]byte(toolsListRequest))

			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.EqualValues(t, 403, resp.StatusCode)
			require.Contains(t, string(resp.Data), "not allowed to access")
			host.CompleteHttp()
		})

		t.Run("readers group member -> continue + consumer header", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-ccc"},
			))
			action := host.CallOnHttpRequestBody([]byte(toolsListRequest))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())

			require.True(t, test.HasHeaderWithValue(host.GetRequestHeaders(), "x-mse-consumer", "alice/sk-ccc"))
			host.CompleteHttp()
		})

		t.Run("direct apikey grant (not in user_apikeys) -> continue", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			// sk-zzz 直授，但不属于任何 user -> consumer=anonymous/sk-zzz
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"x-api-key", "sk-zzz"},
			))
			action := host.CallOnHttpRequestBody([]byte(toolsListRequest))
			require.Equal(t, types.ActionContinue, action)
			require.True(t, test.HasHeaderWithValue(host.GetRequestHeaders(), "x-mse-consumer", "anonymous/sk-zzz"))
			host.CompleteHttp()
		})

		t.Run("route without grants -> continue (not bound)", func(t *testing.T) {
			host, status := test.NewTestHost(noGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders())
			action := host.CallOnHttpRequestBody([]byte(toolsListRequest))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			host.CompleteHttp()
		})
	})
}

// ============================================================================
// 阶段二：工具调用鉴权
// ============================================================================

func TestToolCallAuth(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("readers calls allowed tool -> continue", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-ccc"},
			))
			action := host.CallOnHttpRequestBody(toolsCallRequest("get_weather"))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			host.CompleteHttp()
		})

		t.Run("readers calls disallowed tool -> jsonrpc error", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-ccc"},
			))
			host.CallOnHttpRequestBody(toolsCallRequest("delete_resource"))

			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.Contains(t, string(resp.Data), "is not authorized")
			host.CompleteHttp()
		})

		t.Run("ops wildcard calls any tool -> continue", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			// admin 属于 ops（tool_groups: *）
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-aaa"},
			))
			action := host.CallOnHttpRequestBody(toolsCallRequest("delete_resource"))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			host.CompleteHttp()
		})
	})
}

// ============================================================================
// 阶段三：tools/list 响应过滤
// ============================================================================

func TestToolListFilter(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("readers sees only read-tools", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-ccc"},
			))
			host.CallOnHttpRequestBody([]byte(toolsListRequest))

			host.CallOnHttpResponseHeaders(jsonResponseHeaders())
			host.CallOnHttpResponseBody([]byte(toolsListResponse))

			body := string(host.GetResponseBody())
			require.Contains(t, body, "get_weather")
			require.Contains(t, body, "search")
			require.NotContains(t, body, "delete_resource")
			host.CompleteHttp()
		})

		t.Run("ops wildcard sees all tools", func(t *testing.T) {
			host, status := test.NewTestHost(readersGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-aaa"},
			))
			host.CallOnHttpRequestBody([]byte(toolsListRequest))

			host.CallOnHttpResponseHeaders(jsonResponseHeaders())
			host.CallOnHttpResponseBody([]byte(toolsListResponse))

			body := string(host.GetResponseBody())
			require.Contains(t, body, "get_weather")
			require.Contains(t, body, "search")
			require.Contains(t, body, "delete_resource")
			host.CompleteHttp()
		})
	})
}

// ============================================================================
// 只读发现凭证：list_only —— 可见全量 tools/list，但不可调用任何工具
// ============================================================================

func TestListOnlyDiscovery(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("discovery key passes identity gate", func(t *testing.T) {
			host, status := test.NewTestHost(discoveryGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-mcp-tools-discovery"},
			))
			action := host.CallOnHttpRequestBody([]byte(toolsListRequest))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse()) // 未被 401/403 拦截
			require.True(t, test.HasHeaderWithValue(host.GetRequestHeaders(), "x-mse-consumer", "anonymous/iscovery"))
			host.CompleteHttp()
		})

		t.Run("discovery key sees ALL tools in tools/list", func(t *testing.T) {
			host, status := test.NewTestHost(discoveryGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-mcp-tools-discovery"},
			))
			host.CallOnHttpRequestBody([]byte(toolsListRequest))

			host.CallOnHttpResponseHeaders(jsonResponseHeaders())
			host.CallOnHttpResponseBody([]byte(toolsListResponse))

			body := string(host.GetResponseBody())
			require.Contains(t, body, "get_weather")
			require.Contains(t, body, "search")
			require.Contains(t, body, "delete_resource") // 全量可见，不过滤
			host.CompleteHttp()
		})

		t.Run("discovery key cannot call any tool", func(t *testing.T) {
			host, status := test.NewTestHost(discoveryGrantConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"authorization", "Bearer sk-mcp-tools-discovery"},
			))
			// 即便是只读工具也不许调用。
			host.CallOnHttpRequestBody(toolsCallRequest("get_weather"))

			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.Contains(t, string(resp.Data), "is not authorized")
			host.CompleteHttp()
		})
	})
}

// ============================================================================
// 向后兼容：旧 allow 列表
// ============================================================================

func TestLegacyAllowConfig(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("legacy allow grants full access", func(t *testing.T) {
			host, status := test.NewTestHost(legacyAllowConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.SetRouteName(matchRoute)
			host.CallOnHttpRequestHeaders(jsonRequestHeaders(
				[2]string{"x-api-key", "ak-123"},
			))
			action := host.CallOnHttpRequestBody(toolsCallRequest("delete_resource"))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			host.CompleteHttp()
		})
	})
}
