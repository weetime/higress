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
	"github.com/tidwall/gjson"
)

const identityJSON = `{
  "identity_inject": {
    "enabled": true,
    "cache": {"redis":{"service_name":"redis-master.dns","service_port":6379,"database":1,"timeout":1000},"key_prefix":"mcp:uattr:","ttl":300,"ttl_jitter":60,"negative_ttl":60,"local_ttl":10},
    "origin": {"service_name":"auc.dns","service_port":8002,"host":"auc.rise-vast-system.svc.cluster.local","path":"/v1/user/gateway-attributes","timeout":500,"token_header":"x-gateway-token","token":"secret-1"},
    "on_error": "deny",
    "headers": {"X-User-Email":"{email}","X-Consumer-Id":"{username}:{user_id}"}
  },
  "upstream_auth": {"header":"Authorization","value_prefix":"Bearer ","value":"upstream-key"}
}`

func TestParseIdentityInject(t *testing.T) {
	got := parseIdentityInject(gjson.Parse(identityJSON))
	require.True(t, got.Enabled)
	require.Equal(t, "deny", got.OnError)
	require.Equal(t, 1, got.Cache.Database)
	require.Equal(t, "mcp:uattr:", got.Cache.KeyPrefix)
	require.Equal(t, 300, got.Cache.TTL)
	require.Equal(t, 60, got.Cache.TTLJitter)
	require.Equal(t, 60, got.Cache.NegativeTTL)
	require.True(t, got.Cache.Enabled)
	require.Equal(t, "/v1/user/gateway-attributes", got.Origin.Path)
	require.Equal(t, uint32(500), got.Origin.Timeout)
	require.Equal(t, "{email}", got.Headers["x-user-email"])
}

// 开关缺省必须是 false —— 已上线路由不能因为升级插件就开始注入身份头。
func TestIdentityInjectDefaultsDisabled(t *testing.T) {
	got := parseIdentityInject(gjson.Parse(`{}`))
	require.False(t, got.Enabled)
	require.False(t, got.Cache.Enabled)
	require.Empty(t, got.Headers)
}

// 不配 cache 段 → 纯回源，而不是报错或禁用整个功能。
func TestIdentityInjectWithoutCache(t *testing.T) {
	got := parseIdentityInject(gjson.Parse(`{"identity_inject":{"enabled":true,"origin":{"service_name":"auc.dns","path":"/p"},"headers":{"X-U":"{username}"}}}`))
	require.True(t, got.Enabled)
	require.False(t, got.Cache.Enabled)
	require.Equal(t, uint32(500), got.Origin.Timeout, "origin timeout 必须有非零默认值，否则回调可能永不触发")
}

// on_error 非法值必须落到 deny（fail close），不能落到放行。
// 注意包在 TestHost 里：缺 origin 会走 log.Errorf，而日志宿主未就绪时 proxywasm
// 会 nil deref（与 TestReservedIdentityHeadersAreDropped 同因）。
func TestIdentityInjectOnErrorDefaultsDeny(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		require.Equal(t, "deny", parseIdentityInject(gjson.Parse(`{"identity_inject":{"enabled":true,"on_error":"whatever"}}`)).OnError)
		require.Equal(t, "pass", parseIdentityInject(gjson.Parse(`{"identity_inject":{"enabled":true,"on_error":"pass"}}`)).OnError)
	})
}

// on_incomplete 缺省必须是 deny（fail close），非法值也落 deny。
func TestOnIncompleteDefaultsDeny(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.Equal(t, "deny", parseIdentityInject(gjson.Parse(`{"identity_inject":{"enabled":true}}`)).OnIncomplete)
	require.Equal(t, "deny", parseIdentityInject(gjson.Parse(`{"identity_inject":{"enabled":true,"on_incomplete":"whatever"}}`)).OnIncomplete)
		require.Equal(t, "skip", parseIdentityInject(gjson.Parse(`{"identity_inject":{"enabled":true,"on_incomplete":"skip"}}`)).OnIncomplete)
	})
}

// 保留名与上游认证头同名的身份 header 必须被丢弃——否则管理员一个笔误就能
// 打乱限流归属 / 让 tools/list 过滤失效 / 覆盖上游固定 Key。
//
// 必须包在 RunGoTest + NewTestHost 里：丢弃时会 log.Warnf，而日志宿主未就绪时
// proxywasm.GetProperty 会 nil deref（与 TestParseOverrideFullRuleConfig 同样的原因）。
func TestReservedIdentityHeadersAreDropped(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		cfg := parseIdentityInject(gjson.Parse(`{
      "upstream_auth": {"header":"Authorization"},
      "identity_inject": {"enabled":true, "headers":{
        "X-Mse-Consumer":"{username}",
        "x-api-key-name":"{username}",
        "X-Envoy-Allow-Mcp-Tools":"{username}",
        "Authorization":"{email}",
        "X-User-Email":"{email}"
      }}}`))
		require.Equal(t, map[string]string{"x-user-email": "{email}"}, cfg.Headers,
			"只应保留非保留名的那一条")
	})
}

func TestManagedHeadersAndReferencedFields(t *testing.T) {
	c := parseIdentityInject(gjson.Parse(identityJSON))
	require.ElementsMatch(t, []string{"x-user-email", "x-consumer-id"}, c.managedHeaders())
	require.ElementsMatch(t, []string{"email", "username", "user_id"}, c.referencedFields())
}

func TestParseUpstreamAuth(t *testing.T) {
	got := parseUpstreamAuth(gjson.Parse(identityJSON))
	require.Equal(t, "authorization", got.Header)
	require.Equal(t, "Bearer ", got.ValuePrefix)
	require.Equal(t, "upstream-key", got.Value)
}

// identityRuleConfig 只开 upstream_auth、不开 identity_inject，用于隔离测上游认证与头清理。
func identityRuleConfig() json.RawMessage {
	return ruleConfig(map[string]interface{}{
		"grants": []map[string]interface{}{{"apikey": "sk-aaa", "tool_groups": []string{"*"}}},
		"identity_inject": map[string]interface{}{
			"enabled": false,
			"headers": map[string]string{"X-User-Email": "{email}", "X-Consumer-Id": "{username}:{user_id}"},
		},
		"upstream_auth": map[string]interface{}{"header": "Authorization", "value_prefix": "Bearer ", "value": "upstream-key"},
	})
}

func TestExpandTemplate(t *testing.T) {
	a := userAttrs{"username": "alice", "user_id": "u-1", "email": "a@b.c"}

	got, ok := expandTemplate("{username}:{user_id}", a)
	require.True(t, ok)
	require.Equal(t, "alice:u-1", got)

	// 缺值：不能把 {mobile} 原样透传给上游，也不能悄悄变成空串后照发。
	_, ok = expandTemplate("{mobile}", a)
	require.False(t, ok, "缺值必须报告 false，由调用方决定跳过该 header")

	// 白名单之外的变量不展开，且视为缺值。
	_, ok = expandTemplate("{password}", a)
	require.False(t, ok)

	// 纯字面量没有变量，永远算成功。
	got, ok = expandTemplate("static-value", a)
	require.True(t, ok)
	require.Equal(t, "static-value", got)
}

func TestRenderHeadersSkipsIncompleteTemplates(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		c := parseIdentityInject(gjson.Parse(identityJSON))
		out, complete := c.renderHeaders(userAttrs{"username": "alice", "user_id": "u-1"})
		require.False(t, complete, "缺 email 必须报告 complete=false，供调用方走 on_incomplete")
		// X-Consumer-Id 可展开；X-User-Email 因缺 email 被跳过（而不是注入空值）。
		require.Equal(t, "alice:u-1", out["x-consumer-id"])
		_, exists := out["x-user-email"]
		require.False(t, exists)
	})
}

// covers：属性是否覆盖本路由模板引用的字段（跨路由缓存污染的判据）。
func TestCoversRequiresAllReferencedFields(t *testing.T) {
	c := parseIdentityInject(gjson.Parse(identityJSON)) // 引用 email / username / user_id
	require.True(t, c.covers(userAttrs{"email": "a@b.c", "username": "alice", "user_id": "u-1"}))
	require.False(t, c.covers(userAttrs{"username": "alice", "user_id": "u-1"}), "缺 email 必须判为未覆盖")
	require.False(t, c.covers(userAttrs{"email": "", "username": "alice", "user_id": "u-1"}), "空串等同于缺失")
}

// 开了开关却没配 origin = 配置错误，必须降级为「本路由不注入」，
// 而不是走 on_error(缺省 deny)把整条路由的请求全拒掉。
func TestIdentityInjectWithoutOriginIsDisabled(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(globalOnlyConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		cfg := parseIdentityInject(gjson.Parse(
			`{"identity_inject":{"enabled":true,"on_error":"deny","headers":{"x-kkk":"{user_id}"}}}`))
		require.False(t, cfg.Enabled, "缺 origin 时必须降级为未启用，否则整条路由会被 deny")
	})
}
