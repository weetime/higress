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

	// defaultAuthHeaderName 是 OpenAI 风格凭证头。
	defaultAuthHeaderName = "Authorization"

	// headerOriginalAuth / headerFallbackFrom 与 ai-route-auth 中的同名常量含义一致：
	// 模型降级（fallback）时 Envoy 走 internal_redirect 把请求重新灌进整条 filter chain，
	// 上一趟的 ai-proxy 已经把 Authorization 换成了上游 provider 的 apiToken，
	// 用户的原始凭证被保存在 X-HI-ORIGINAL-AUTH 里。详见 ai-route-auth/main.go:42-50。
	headerOriginalAuth = "X-HI-ORIGINAL-AUTH"
	headerFallbackFrom = "x-higress-fallback-from"
)

// anthropicStyleAuthHeaders 是 Anthropic / 透传风格的凭证头，按优先级排列。
// 顺序必须与 ai-route-auth/main.go 的同名变量保持一致：两个插件解析出的
// apiKey 不一致时，本插件的名单会对着一个「不是它鉴权用的那把 key」做判断。
var anthropicStyleAuthHeaders = []string{"x-api-key", "x-authorization", "anthropic-api-key"}

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

	// blockedApiKeys 余额已耗尽的 API Key 集合，key 是【完整的 apiKey】
	// （与 ai-route-auth 的 user_apikeys 里的 key 同一份值），value 无意义。
	// 与 blockedUsers 是 OR 关系：任一命中即拒绝。
	//
	// 存在的意义是比 username 更细的封禁粒度——同一用户的多把 key 可以单独停用
	// （比如某把 key 泄露后被跑爆、或按 key 而非按人计费的场景）。
	blockedApiKeys map[string]struct{}

	// authHeaderName 指定从哪个请求头读取 API Key，缺省为 Authorization。
	// 【必须与 ai-route-auth 的 auth_header_name 配成同一个值】，否则两个插件
	// 解析出的 apiKey 不同，blocked_api_keys 会静默失效（见 extractApiKey）。
	authHeaderName string
}

// parseConfig 解析 defaultConfig。
//
// Configuration format:
//
//	{
//	  "blocked_users": {
//	    "alice": {},
//	    "bob": {}
//	  },
//	  "blocked_api_keys": {
//	    "sk-xxxxxxxx": {}
//	  },
//	  "auth_header_name": "Authorization"
//	}
//
// 本函数【永不返回 error】。任何解析失败都会让整个插件配置加载不上；
// 而名单由外部计费系统写入，形状不完全可控（value 可能是 null、对象、甚至整个
// 字段被写成数组）。这里只读 key、忽略 value，任何形状都能降级成
// 「名单为空 = 全放行」，不把风险传导到网关。
func parseConfig(json gjson.Result, config *CreditCheckConfig) error {
	config.blockedUsers = parseKeySet(json, "blocked_users", func(u string) {
		// username 中含 "/" 时会被 extractUsername 截断，永远匹配不到这条名单项，
		// 属于一种隐蔽的静默失效——提前 Warn 出来，而不是等运营发现拦不住人。
		if strings.Contains(u, "/") {
			log.Warnf("blocked_users 中的 key %q 含有 \"/\"，extractUsername 会在第一个 \"/\" 处截断，"+
				"该名单项永远不会命中，请检查配置", u)
		}
	})

	config.blockedApiKeys = parseKeySet(json, "blocked_api_keys", func(k string) {
		// 本插件比对的是【剥掉 "Bearer " 之后】的裸 token（extractApiKey），
		// 名单里若连前缀一起写进来就永远不会命中。这是最容易犯的一种误配置
		// （直接从抓包/日志里的 Authorization 头整行复制过来），必须显式告警。
		if len(k) >= 7 && strings.EqualFold(k[:7], "bearer ") {
			log.Warnf("blocked_api_keys 中存在以 \"Bearer \" 开头的 key（%s），"+
				"本插件比对的是剥掉该前缀后的裸 token，该名单项永远不会命中，请只填 token 本身",
				maskKey(k))
		}
	})

	// auth_header_name 缺省为 Authorization，与 ai-route-auth 的默认值一致。
	config.authHeaderName = strings.TrimSpace(json.Get("auth_header_name").String())
	if config.authHeaderName == "" {
		config.authHeaderName = defaultAuthHeaderName
	}

	log.Infof("loaded %d blocked users, %d blocked api keys",
		len(config.blockedUsers), len(config.blockedApiKeys))
	return nil
}

// parseKeySet 把 field 指向的 JSON 对象读成一个「只看 key」的集合。
// check 是可选的逐 key 校验回调，只负责打告警，不影响解析结果。
//
// 注意：JSON 数组是一种特别隐蔽的畸形——gjson 的 ForEach 会把数组当成可迭代对象，
// key 拿到的是数组下标（"0"、"1"...）而不是元素值，如果不特判会把
// {"blocked_users":["alice","bob"]} 静默解析成名单 {"0","1"}：alice、bob 实际上
// 没被拉黑，还会打一条「loaded 2 blocked users」看似正常的日志，把最自然的一种
// 误配置（把名单写成 JSON 数组）伪装成了成功。这里显式拒绝非对象形状，并 Warn 出来。
func parseKeySet(json gjson.Result, field string, check func(key string)) map[string]struct{} {
	set := make(map[string]struct{})
	v := json.Get(field)
	if v.Exists() && !v.IsObject() {
		log.Warnf("%s 不是对象（实际为 %s），按空名单处理 —— 请检查配置格式", field, v.Type)
		return set
	}
	v.ForEach(func(key, _ gjson.Result) bool {
		k := strings.TrimSpace(key.String())
		if k == "" {
			return true
		}
		if check != nil {
			check(k)
		}
		set[k] = struct{}{}
		return true
	})
	return set
}

// ============================================================================
// Request Processing
// ============================================================================

// onHttpRequestHeaders 检查调用方是否处于欠费名单中。
//
// 两个维度是 OR 关系：username 命中 blocked_users，或 apiKey 命中 blocked_api_keys，
// 都返回 402。判定顺序刻意把「两个名单都为空」放在最前：这是日常绝大多数请求的路径，
// 提前返回可以连 header 都不用读；随后先判 username（只读一个 header），
// 再判 apiKey（可能要读多个 header）。
func onHttpRequestHeaders(ctx wrapper.HttpContext, config CreditCheckConfig) types.Action {
	// 名单为空 = 没人欠费，直接放行。
	if len(config.blockedUsers) == 0 && len(config.blockedApiKeys) == 0 {
		return types.ActionContinue
	}

	if len(config.blockedUsers) > 0 {
		// 拿不到 consumer 说明上游没写（路由不在 ai-route-auth 的 matchRules 里），
		// 无身份可判，跳过这一维度。
		if raw, err := proxywasm.GetHttpRequestHeader(consumerHeader); err == nil {
			username := extractUsername(raw)
			// 空 username 说明 consumer 值畸形（如 "/sk-xxx"），没有可用身份可判——
			// 这种情况按「没有 header」同等对待，放行而非拦截。
			// （parseKeySet 已对 key 做 TrimSpace 并丢弃空 key，blockedUsers 不可能含 ""，
			// 这里是防御性写法，不依赖该保证。）
			if username != "" {
				if _, blocked := config.blockedUsers[username]; blocked {
					log.Warnf("denied: user %q has insufficient credit", username)
					return deniedInsufficientCredit()
				}
			}
		}
	}

	if len(config.blockedApiKeys) > 0 {
		// 拿不到凭证（所有候选 header 都缺失）同样跳过——没有 key 可判就不是本维度的事。
		if apiKey, found := extractApiKey(config.authHeaderName); found {
			if _, blocked := config.blockedApiKeys[apiKey]; blocked {
				log.Warnf("denied: api key %s has insufficient credit", maskKey(apiKey))
				return deniedInsufficientCredit()
			}
		}
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

// extractApiKey 从请求头中提取【完整的】原始 API Key。
//
// 为什么这里拿得到：本插件是 AUTHZ/690，跑在 ai-route-auth(AUTHZ/700) 之后、
// ai-proxy 之前。ai-route-auth 只【新增】x-mse-consumer / x-api-key-name，
// 不会删改客户端带来的凭证头；把 Authorization 换成上游 provider apiToken 的是
// 更靠后的 ai-proxy。所以在本插件执行的这一刻，客户端的原始凭证仍然原样在头里。
// （x-mse-consumer 里只有 apiKey 的后 8 位，不足以做完整比对，因此不用它。）
//
// 优先级与 ai-route-auth/main.go 的 extractCredential 严格对齐——两边不一致就会
// 出现「ai-route-auth 按 key A 鉴权、本插件按 key B 查名单」的静默失效：
//  0. internal_redirect 重入时的 X-HI-ORIGINAL-AUTH（仅在 x-higress-fallback-from
//     存在时才信任；该 header 不防伪造，首跳一律以下面的顺序为准）
//  1. 显式配置的 auth_header_name（非默认值时）
//  2. x-api-key / x-authorization / anthropic-api-key（Anthropic / 透传风格）
//  3. Authorization: Bearer <key>（OpenAI 风格；无 Bearer 前缀时按原值处理）
//
// 返回 (apiKey, found)。found=false 表示所有候选 header 均缺失或为空。
func extractApiKey(configuredHeader string) (string, bool) {
	if from, err := proxywasm.GetHttpRequestHeader(headerFallbackFrom); err == nil && from != "" {
		if v, err := proxywasm.GetHttpRequestHeader(headerOriginalAuth); err == nil {
			if key := extractBearerToken(v); key != "" {
				return key, true
			}
		}
	}

	if configuredHeader != "" && !strings.EqualFold(configuredHeader, defaultAuthHeaderName) {
		if v, err := proxywasm.GetHttpRequestHeader(configuredHeader); err == nil {
			if key := strings.TrimSpace(v); key != "" {
				return key, true
			}
		}
	}

	for _, h := range anthropicStyleAuthHeaders {
		if v, err := proxywasm.GetHttpRequestHeader(h); err == nil {
			if key := strings.TrimSpace(v); key != "" {
				return key, true
			}
		}
	}

	if v, err := proxywasm.GetHttpRequestHeader(defaultAuthHeaderName); err == nil {
		if key := extractBearerToken(v); key != "" {
			return key, true
		}
	}

	return "", false
}

// extractBearerToken 从 Authorization 头中提取 token。
// 兼容 "Bearer <token>" 与直接给出 token 两种写法（与 ai-route-auth 一致）。
func extractBearerToken(headerValue string) string {
	v := strings.TrimSpace(headerValue)
	if len(v) >= 7 && strings.EqualFold(v[:7], "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// maskKey 打日志用的脱敏形式：只保留后 8 位，与 x-mse-consumer 里的后缀一致，
// 方便运维把日志和名单项对上，同时不把完整凭证写进访问日志。
func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return "***" + key[len(key)-8:]
}

// ============================================================================
// Response Helpers
// ============================================================================

// deniedInsufficientCredit 余额不足时返回 402。
//
// 用 402 而非 401/403，是为了让客户端能把「没钱」和「没权限」区分开；
// 不用 429 是因为现成 SDK 会对 429 自动重试，欠费重试没有意义只会放大压力。
// 响应体采用 OpenAI 兼容的 error 结构，下游 AI 客户端可直接解析。
//
// user 维度与 apiKey 维度共用同一个响应（含 StatusCodeDetail）：对外不区分是
// 「人欠费」还是「这把 key 停用」，避免把内部计费口径泄露给调用方；
// 需要区分时看网关日志里的 Warn（那里分别打了 user / masked key）。
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
