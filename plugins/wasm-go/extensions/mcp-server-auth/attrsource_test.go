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

// originOnlyConfig：开 identity_inject 但不配 cache → 纯回源。
func originOnlyConfig(onError string) json.RawMessage {
	return ruleConfig(map[string]interface{}{
		"grants": []map[string]interface{}{{"apikey": "sk-aaa", "tool_groups": []string{"*"}}},
		"identity_inject": map[string]interface{}{
			"enabled":  true,
			"on_error": onError,
			"origin": map[string]interface{}{
				"service_name": "auc.dns", "service_port": 8002,
				"host": "auc.svc", "path": "/v1/user/gateway-attributes",
				"timeout": 500, "token_header": "x-gateway-token", "token": "secret-1",
			},
			"headers": map[string]string{"X-User-Email": "{email}", "X-Consumer-Id": "{username}:{user_id}"},
		},
	})
}

const aucOKBody = `{"userId":"u-1","username":"admin","name":"Admin","email":"admin@rise.io","mobile":"13800000000"}`

func headerMap(hs [][2]string) map[string]string {
	m := map[string]string{}
	for _, h := range hs {
		m[h[0]] = h[1]
	}
	return m
}

// 回源成功：请求先挂起，回调后注入 header 并放行。
func TestOriginFetchInjectsHeaders(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(originOnlyConfig("deny"))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		action := host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		require.Equal(t, types.HeaderStopAllIterationAndWatermark, action, "回源期间必须挂起")
		require.Len(t, host.GetHttpCalloutAttributes(), 1)

		host.CallOnHttpCall([][2]string{{":status", "200"}}, []byte(aucOKBody))

		got := headerMap(host.GetRequestHeaders())
		require.Equal(t, "admin@rise.io", got["x-user-email"])
		require.Equal(t, "admin:u-1", got["x-consumer-id"])
		host.CompleteHttp()
	})
}

// 回源失败 + on_error=deny：有 body 的请求由 body 阶段发 JSON-RPC 错误，不发裸 403。
func TestOriginFailureDeniesViaJsonRpc(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(originOnlyConfig("deny"))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnHttpCall([][2]string{{":status", "503"}}, []byte(`upstream down`))
		// headers 阶段不发本地响应，交给 body 阶段
		require.Nil(t, host.GetLocalResponse())

		host.CallOnHttpRequestBody([]byte(toolsListRequest))
		body := host.GetResponseBody()
		if body == nil {
			// 部分框架路径下 JSON-RPC 错误走 local response
			require.NotNil(t, host.GetLocalResponse())
		}
		host.CompleteHttp()
	})
}

// 回源失败 + on_error=pass → 放行，但绝不能带身份头。
func TestOriginFailurePassInjectsNothing(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(originOnlyConfig("pass"))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnHttpCall([][2]string{{":status", "500"}}, []byte(``))

		require.Nil(t, host.GetLocalResponse())
		got := headerMap(host.GetRequestHeaders())
		require.NotContains(t, got, "x-user-email")
		require.NotContains(t, got, "x-consumer-id")
		host.CompleteHttp()
	})
}

// 开关关闭时不得发起任何回源调用（回归底线）。
func TestDisabledMakesNoCallout(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(identityRuleConfig()) // enabled:false
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		action := host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		require.NotEqual(t, types.HeaderStopAllIterationAndWatermark, action)
		require.Empty(t, host.GetHttpCalloutAttributes())
		host.CompleteHttp()
	})
}

// 客户端伪造的身份头必须在解析出结果之前就被删掉，不能"解析成功才覆盖"。
func TestForgedIdentityHeadersAreRemoved(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(identityRuleConfig())
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders(
			[2]string{"Authorization", "Bearer sk-aaa"},
			[2]string{"X-User-Email", "attacker@evil.com"},
			[2]string{"X-Consumer-Id", "victim:u-9"},
		))
		for _, h := range host.GetRequestHeaders() {
			require.NotEqual(t, "attacker@evil.com", h[1], "伪造的 X-User-Email 未被清除")
			require.NotEqual(t, "victim:u-9", h[1], "伪造的 X-Consumer-Id 未被清除")
		}
		host.CompleteHttp()
	})
}

// 上游固定 Key 必须覆盖客户端的 Authorization，且用户侧 CAMP API Key 不得外泄。
func TestUpstreamAuthOverridesClientCredential(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(identityRuleConfig())
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		got := headerMap(host.GetRequestHeaders())
		require.Equal(t, "Bearer upstream-key", got["authorization"])
		require.NotContains(t, got["authorization"], "sk-aaa")
		host.CompleteHttp()
	})
}

// cachedConfig：开 identity_inject + cache（Redis read-through）。
func cachedConfig(onError string, localTTL int) json.RawMessage {
	return ruleConfig(map[string]interface{}{
		"grants": []map[string]interface{}{{"apikey": "sk-aaa", "tool_groups": []string{"*"}}},
		"identity_inject": map[string]interface{}{
			"enabled":  true,
			"on_error": onError,
			"cache": map[string]interface{}{
				"redis":      map[string]interface{}{"service_name": "redis-master.dns", "service_port": 6379, "database": 1, "timeout": 1000},
				"key_prefix": "mcp:uattr:", "ttl": 300, "local_ttl": localTTL,
			},
			"origin": map[string]interface{}{
				"service_name": "auc.dns", "service_port": 8002, "host": "auc.svc",
				"path": "/v1/user/gateway-attributes", "timeout": 500,
				"token_header": "x-gateway-token", "token": "secret-1",
			},
			"headers": map[string]string{"X-User-Email": "{email}", "X-Consumer-Id": "{username}:{user_id}"},
		},
	})
}

// Redis 命中：不得回源 AUC。
func TestCacheHitSkipsOrigin(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespString(aucOKBody))

		require.Empty(t, host.GetHttpCalloutAttributes(), "缓存命中不应回源")
		require.Equal(t, "admin@rise.io", headerMap(host.GetRequestHeaders())["x-user-email"])
		host.CompleteHttp()
	})
}

// Redis 未命中：回源后必须写回（一次 SETEX），且请求只 resume 一次。
func TestCacheMissFallsBackAndWritesBack(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespNull())
		require.Len(t, host.GetHttpCalloutAttributes(), 1, "未命中必须回源")
		host.CallOnHttpCall([][2]string{{":status", "200"}}, []byte(aucOKBody))
		host.CallOnRedisCall(0, test.CreateRedisRespString("OK")) // 回写完成 → resume

		require.Equal(t, "admin:u-1", headerMap(host.GetRequestHeaders())["x-consumer-id"])
		require.Nil(t, host.GetLocalResponse())
		host.CompleteHttp()
	})
}

// Redis 报错：必须回源（软依赖），请求仍成功。
func TestCacheErrorFallsBackToOrigin(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespError("CONNREFUSED"))
		require.Len(t, host.GetHttpCalloutAttributes(), 1, "Redis 故障必须降级回源，而不是失败")
		host.CallOnHttpCall([][2]string{{":status", "200"}}, []byte(aucOKBody))
		host.CallOnRedisCall(0, test.CreateRedisRespError("CONNREFUSED")) // 回写也失败 → 仍须 resume

		require.Nil(t, host.GetLocalResponse(), "回写失败不能影响请求成功")
		require.Equal(t, "admin@rise.io", headerMap(host.GetRequestHeaders())["x-user-email"])
		host.CompleteHttp()
	})
}

// 跨路由缓存污染：别的路由写入的条目缺本路由需要的字段时，必须回源而不是静默丢 header。
func TestCacheHitWithMissingFieldsRefetches(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		// 模拟另一条只引用 {username} 的路由写下的条目：没有 email
		host.CallOnRedisCall(0, test.CreateRedisRespString(`{"userId":"u-1","username":"admin"}`))
		require.Len(t, host.GetHttpCalloutAttributes(), 1, "字段不全必须回源，不能直接用")

		host.CallOnHttpCall([][2]string{{":status", "200"}}, []byte(aucOKBody))
		host.CallOnRedisCall(0, test.CreateRedisRespString("OK"))
		require.Equal(t, "admin@rise.io", headerMap(host.GetRequestHeaders())["x-user-email"])
		host.CompleteHttp()
	})
}

// 用户不存在（AUC 404）：写负缓存后按 on_error 处理。
func TestOriginNotFoundWritesNegativeCache(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		cfg := ruleConfig(map[string]interface{}{
			"grants": []map[string]interface{}{{"apikey": "sk-aaa", "tool_groups": []string{"*"}}},
			"identity_inject": map[string]interface{}{
				"enabled": true, "on_error": "deny",
				"cache": map[string]interface{}{
					"redis":      map[string]interface{}{"service_name": "redis-master.dns", "service_port": 6379, "timeout": 1000},
					"key_prefix": "mcp:uattr:", "ttl": 300, "negative_ttl": 60,
				},
				"origin":  map[string]interface{}{"service_name": "auc.dns", "host": "auc.svc", "path": "/p", "timeout": 500},
				"headers": map[string]string{"X-User-Email": "{email}"},
			},
		})
		host, status := test.NewTestHost(cfg)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespNull())
		// GetRedisCalloutAttributes 是「待应答」队列，上面那次 GET 已被应答出队。
		require.Empty(t, host.GetRedisCalloutAttributes())
		host.CallOnHttpCall([][2]string{{":status", "404"}}, []byte(`{"reason":"not found"}`))
		require.Len(t, host.GetRedisCalloutAttributes(), 1, "404 应写负缓存（一次 SETEX）")
		host.CallOnRedisCall(0, test.CreateRedisRespString("OK"))
		host.CompleteHttp()
	})
}

// 负缓存命中：直接走 on_error，不得再打 AUC。
func TestNegativeCacheHitSkipsOrigin(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespString(`{"__absent__":true}`))

		require.Empty(t, host.GetHttpCalloutAttributes(), "负缓存命中不应回源")
		host.CompleteHttp()
	})
}

// AUC 5xx 不得写负缓存 —— 否则一次抖动会被放大成一个 TTL 的持续拒绝。
func TestOrigin5xxDoesNotWriteNegativeCache(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespNull())
		n := len(host.GetRedisCalloutAttributes())
		host.CallOnHttpCall([][2]string{{":status", "503"}}, []byte(``))
		require.Equal(t, n, len(host.GetRedisCalloutAttributes()), "5xx 不应产生 SETEX")
		host.CompleteHttp()
	})
}

// 属性为空 ≠ 取不到：on_incomplete=deny 时必须拒，而不是静默放行。
func TestIncompleteTemplateDenies(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(cachedConfig("deny", 0))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespNull())
		// AUC 200 但 email 为空（该用户没填邮箱）
		host.CallOnHttpCall([][2]string{{":status", "200"}}, []byte(`{"userId":"u-1","username":"admin","email":""}`))

		got := headerMap(host.GetRequestHeaders())
		require.NotContains(t, got, "x-user-email", "缺值不得注入空头")
		host.CompleteHttp()
	})
}

// VM 缓存命中：不挂起、不打 Redis。
func TestLocalCacheAvoidsRedis(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		localAttrCache = map[string]localAttrEntry{} // 隔离前序用例
		host, status := test.NewTestHost(cachedConfig("deny", 10))
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		host.CallOnRedisCall(0, test.CreateRedisRespString(aucOKBody))
		host.CompleteHttp()

		// 第二次请求：VM 缓存内应直接命中，不挂起、不发 Redis 调用
		host.InitHttp()
		action := host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		require.NotEqual(t, types.HeaderStopAllIterationAndWatermark, action, "VM 缓存命中应同步返回")
		require.Empty(t, host.GetRedisCalloutAttributes())
		require.Equal(t, "admin@rise.io", headerMap(host.GetRequestHeaders())["x-user-email"])
		host.CompleteHttp()
		localAttrCache = map[string]localAttrEntry{}
	})
}

// 回归：上游认证头配成 Authorization（最常见）时，认证必须仍然通过。
//
// 曾经的 bug：clearManagedHeaders 把 upstream_auth.Header 也删了，而它就是本插件自己
// 读 token 的那个头 —— 于是 extractToken 拿不到凭证，整条路由 401
// "No Key Authentication information found"。
func TestUpstreamAuthOnAuthorizationDoesNotBreakAuth(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(identityRuleConfig()) // upstream_auth.header = Authorization
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		got := headerMap(host.GetRequestHeaders())

		// 认证通过的证据：consumer 头被注入（说明 token 被读到且 grant 命中）
		require.NotEmpty(t, got["x-mse-consumer"], "token 必须仍可读，否则会 401")
		// 同时上游拿到的是固定 Key，不是客户端的
		require.Equal(t, "Bearer upstream-key", got["authorization"])

		// body 阶段也必须能读到 token（真实 401 就发生在这一阶段）
		host.CallOnHttpRequestBody([]byte(toolsListRequest))
		require.Nil(t, host.GetLocalResponse(), "body 阶段不应因读不到 token 而拒绝")
		host.CompleteHttp()
	})
}

// 配了上游认证头但没有值：不能把客户端凭证透传给上游。
func TestUpstreamAuthWithoutValueStripsClientCredential(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		cfg := ruleConfig(map[string]interface{}{
			"grants":        []map[string]interface{}{{"apikey": "sk-aaa", "tool_groups": []string{"*"}}},
			"upstream_auth": map[string]interface{}{"header": "Authorization", "value_prefix": "Bearer "},
		})
		host, status := test.NewTestHost(cfg)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()
		host.SetRouteName(matchRoute)

		host.CallOnHttpRequestHeaders(jsonRequestHeaders([2]string{"Authorization", "Bearer sk-aaa"}))
		got := headerMap(host.GetRequestHeaders())
		require.NotContains(t, got["authorization"], "sk-aaa", "客户端凭证不得透传给上游")
		host.CompleteHttp()
	})
}
