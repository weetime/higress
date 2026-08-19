// Copyright (c) 2022 Alibaba Group Holding Ltd.
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
	"net/http"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

// ============================================================================
// Constants
// ============================================================================

const (
	pluginName = "ai-credit-check"

	// consumerHeader 由上游 ai-route-auth / mcp-server-auth 写入，值的格式是
	// "<username>/<apiKey 后 8 位>"（见 ai-route-auth/main.go:349）。
	// 注意该 header 只在 ai-route-auth 命中 matchRule 的路由上可信，
	// 覆盖面与安全边界详见设计文档 §6。
	consumerHeader = "x-mse-consumer"
)

// ============================================================================
// Plugin Entry Points
// ============================================================================

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

// ============================================================================
// Configuration
// ============================================================================

// CreditCheckConfig 余额门禁配置。
type CreditCheckConfig struct {
	// blockedUsers 余额已耗尽（<= 0）的用户名集合。
	// 配置里是一个 map，key 是 username，value 无意义（约定写 {}）。
	// 用 map 而非数组是为了让计费系统能用 JSON Merge Patch 单独增删一个 key，
	// 免去 read-modify-write 带来的并发丢更新。
	blockedUsers map[string]struct{}
}

// parseConfig 解析 defaultConfig。
//
// Configuration format:
//
//	{
//	  "blocked_users": {
//	    "alice": {},
//	    "bob": {}
//	  }
//	}
//
// 本函数【永不返回 error】。任何解析失败都会让整个插件配置加载不上；
// 而 blocked_users 由外部计费系统写入，形状不完全可控（value 可能是 null、
// 对象、甚至整个字段被写成数组）。这里只读 key、忽略 value，任何形状都能降级成
// 「名单为空 = 全放行」，不把风险传导到网关。
//
// 注意：JSON 数组是一种特别隐蔽的畸形——gjson 的 ForEach 会把数组当成可迭代对象，
// key 拿到的是数组下标（"0"、"1"...）而不是元素值，如果不特判会把
// {"blocked_users":["alice","bob"]} 静默解析成名单 {"0","1"}：alice、bob 实际上
// 没被拉黑，还会打一条「loaded 2 blocked users」看似正常的日志，把最自然的一种
// 误配置（把名单写成 JSON 数组）伪装成了成功。这里显式拒绝非对象形状，并 Warn 出来。
func parseConfig(json gjson.Result, config *CreditCheckConfig) error {
	config.blockedUsers = make(map[string]struct{})
	blocked := json.Get("blocked_users")
	if blocked.Exists() && !blocked.IsObject() {
		log.Warnf("blocked_users 不是对象（实际为 %s），按空名单处理 —— 请检查配置格式", blocked.Type)
	} else {
		blocked.ForEach(func(key, _ gjson.Result) bool {
			u := strings.TrimSpace(key.String())
			if u == "" {
				return true
			}
			// username 中含 "/" 时会被 extractUsername 截断，永远匹配不到这条名单项，
			// 属于第二种隐蔽的静默失效——提前 Warn 出来，而不是等运营发现拦不住人。
			if strings.Contains(u, "/") {
				log.Warnf("blocked_users 中的 key %q 含有 \"/\"，extractUsername 会在第一个 \"/\" 处截断，"+
					"该名单项永远不会命中，请检查配置", u)
			}
			config.blockedUsers[u] = struct{}{}
			return true
		})
	}
	log.Infof("loaded %d blocked users", len(config.blockedUsers))
	return nil
}

// ============================================================================
// Request Processing
// ============================================================================

// onHttpRequestHeaders 检查调用方是否处于欠费名单中。
//
// 判定顺序刻意把「名单为空」放在最前：这是日常绝大多数请求的路径，
// 提前返回可以连 header 都不用读。
func onHttpRequestHeaders(ctx wrapper.HttpContext, config CreditCheckConfig) types.Action {
	// 名单为空 = 没人欠费，直接放行。
	if len(config.blockedUsers) == 0 {
		return types.ActionContinue
	}

	// 拿不到 consumer 说明上游没写（路由不在 ai-route-auth 的 matchRules 里），
	// 无身份可判，放行。
	raw, err := proxywasm.GetHttpRequestHeader(consumerHeader)
	if err != nil {
		return types.ActionContinue
	}

	username := extractUsername(raw)
	// 空 username 说明 consumer 值畸形（如 "/sk-xxx"），没有可用身份可判——
	// 这种情况按「没有 header」同等对待，放行而非拦截。
	// （parseConfig 已对 key 做 TrimSpace 并丢弃空 key，blockedUsers 不可能含 ""，
	// 这里是防御性写法，不依赖该保证。）
	if username == "" {
		return types.ActionContinue
	}

	if _, blocked := config.blockedUsers[username]; blocked {
		log.Warnf("denied: user %q has insufficient credit", username)
		return deniedInsufficientCredit()
	}

	return types.ActionContinue
}

// extractUsername 从 x-mse-consumer 中切出用户名。
// 上游写入的格式是 "<username>/<apiKey 后 8 位>"（ai-route-auth/main.go:349），
// 不含 "/" 时整段当作用户名，兼容可能直接写纯用户名的其它上游插件。
func extractUsername(consumer string) string {
	if i := strings.Index(consumer, "/"); i >= 0 {
		consumer = consumer[:i]
	}
	return strings.TrimSpace(consumer)
}

// ============================================================================
// Response Helpers
// ============================================================================

// deniedInsufficientCredit 余额不足时返回 402。
//
// 用 402 而非 401/403，是为了让客户端能把「没钱」和「没权限」区分开；
// 不用 429 是因为现成 SDK 会对 429 自动重试，欠费重试没有意义只会放大压力。
// 响应体采用 OpenAI 兼容的 error 结构，下游 AI 客户端可直接解析。
func deniedInsufficientCredit() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusPaymentRequired,
		pluginName+".insufficient_credit",
		[][2]string{{"Content-Type", "application/json"}},
		[]byte(`{"error":{"message":"Insufficient credit. Please top up your account.","type":"insufficient_quota","code":"insufficient_quota"}}`),
		-1,
	)
	return types.ActionContinue
}
