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
	"fmt"
	"net/http"
	"strings"

	"github.com/alibaba/higress/plugins/wasm-go/pkg/mcp"
	"github.com/alibaba/higress/plugins/wasm-go/pkg/mcp/utils"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	protectionSpace = "MSE Gateway" // 认证失败时返回响应头 WWW-Authenticate: Key realm=MSE Gateway

	// defaultAuthHeaderName 当未配置 keys 时回退的 token 来源头。
	defaultAuthHeaderName = "Authorization"

	// wildcard 通配符：grant.tool_groups 含 "*" 表示该主体可调用本 server 全部工具。
	wildcard = "*"

	// ctxAllowedListGroups 在请求阶段写入、在 tools/list 阶段读取，
	// 保存当前调用者可【看到】的工具集名称列表（[]string）。
	ctxAllowedListGroups = "mcp_auth_allowed_list_groups"
	// ctxAllowedCallGroups 在请求阶段写入、在 tools/call 阶段读取，
	// 保存当前调用者可【调用】的工具集名称列表（[]string）。
	ctxAllowedCallGroups = "mcp_auth_allowed_call_groups"
	// ctxRequestToken 在请求头阶段写入：该请求携带的【用户】凭证。
	// 必须存下来——上游固定 Key 会在请求头阶段的 defer 里覆盖 Authorization，
	// 之后 body 阶段再读那个头拿到的是上游 Key，门禁会把每个请求都判成未授权(403)。
	ctxRequestToken = "mcp_auth_request_token"
)

// token 提取状态
const (
	tokenOK = iota
	tokenMissing
	tokenMulti
)

func main() {}

func init() {
	mcp.LoadMCPFilter(
		mcp.FilterName("mcp-server-auth"),
		mcp.SetConfigOverrideParser(parseGlobalConfig, parseOverrideRuleConfig),
		// 阶段零：请求头阶段按 api-key 注入 x-envoy-allow-mcp-tools（此阶段改请求头才能传到 mcp-server host，
		// 供 OPEN_API 自托管 tools/list 自过滤；body 阶段改头传不到 host）。
		mcp.SetRequestHeadersFilter(onRequestHeaders),
		// 阶段一：身份解析 + server 访问门禁（每个 JSON-RPC 请求）
		mcp.SetJsonRpcRequestFilter(onJsonRpcRequest),
		// 阶段二：工具调用鉴权（仅 tools/call）
		mcp.SetToolCallRequestFilter(onToolCall),
		// 阶段三：tools/list 响应过滤
		mcp.SetToolListResponseFilter(onToolListResponse),
		// 兜底：非 JSON-RPC 请求（如 SSE 建链）施加同样的 server 访问门禁
		mcp.SetFallbackHTTPRequestFilter(onFallbackHttpRequest),
	)
	mcp.InitMCPFilter()
}

// ============================================================================
// 配置类型
// ============================================================================

// Grant 一条路由级授权：主体（group 或 apikey 二选一）可调用的工具集列表。
type Grant struct {
	Group      string   `json:"group,omitempty"`
	Apikey     string   `json:"apikey,omitempty"`
	ToolGroups []string `json:"tool_groups"`
	// ListOnly true：该 grant 的 tool_groups 只进 tools/list 可见集，不进 tools/call 可调用集。
	// 用于「只读发现」凭证（可看全量工具、不能调用）。缺省 false → 行为与旧配置完全一致。
	ListOnly bool `json:"list_only,omitempty"`
}

// McpAuthConfig 鉴权配置。
//   - 全局字典（实例级）：Keys / apiKeyToUser / userToGroups / toolGroups
//   - 路由级授权：grants
type McpAuthConfig struct {
	// Keys token 的来源请求头名称（Authorization 走 Bearer <token>）。
	Keys []string
	// apiKeyToUser apikey -> user（由 user_apikeys 反转而来，O(1) 反查）。
	apiKeyToUser map[string]string
	// userToGroups user -> 所属用户组（由 group_users 反转而来）。
	userToGroups map[string][]string
	// toolGroups 工具集名 -> 工具名列表。
	toolGroups map[string][]string

	// grants 本路由（=一个 MCP server）的授权列表（主体 -> 工具集）。
	grants []Grant

	// identity 用户身份 Header 注入（缺省 Enabled=false，行为与改动前一致）。
	identity IdentityInject
	// upstreamAuth 上游固定认证头。
	upstreamAuth UpstreamAuth
	// redisClient 属性缓存客户端。nil 表示缓存不可用（退化为纯回源，Redis 是软依赖）。
	redisClient wrapper.RedisClient
}

// ============================================================================
// 配置解析
// ============================================================================

// parseDicts 从给定 JSON root 解析身份与工具字典到 config（增量覆盖）。
// keys 若存在则整体替换；user_apikeys/group_users/tool_groups 合并进已有 map。
// 同时被 parseGlobalConfig（全局）与 parseOverrideRuleConfig（规则级）复用，
// 使整套配置既可放在 defaultConfig，也可直接放在 matchRules。
func parseDicts(root gjson.Result, config *McpAuthConfig) {
	if keys := root.Get("keys"); keys.Exists() && len(keys.Array()) > 0 {
		config.Keys = nil
		for _, k := range keys.Array() {
			config.Keys = append(config.Keys, k.String())
		}
	}

	// user_apikeys: user -> [apikey]，反转为 apikey -> user
	root.Get("user_apikeys").ForEach(func(user, apikeys gjson.Result) bool {
		for _, ak := range apikeys.Array() {
			config.apiKeyToUser[ak.String()] = user.String()
		}
		return true
	})

	// group_users: group -> [user]，反转为 user -> [group]
	root.Get("group_users").ForEach(func(group, users gjson.Result) bool {
		for _, u := range users.Array() {
			config.userToGroups[u.String()] = append(config.userToGroups[u.String()], group.String())
		}
		return true
	})

	// tool_groups: toolGroup -> [tool]
	root.Get("tool_groups").ForEach(func(tg, tools gjson.Result) bool {
		arr := make([]string, 0, len(tools.Array()))
		for _, t := range tools.Array() {
			arr = append(arr, t.String())
		}
		config.toolGroups[tg.String()] = arr
		return true
	})
}

// parseGlobalConfig 解析实例级全局字典。
func parseGlobalConfig(configBytes []byte, globalConfig *any) error {
	var config McpAuthConfig
	config.apiKeyToUser = make(map[string]string)
	config.userToGroups = make(map[string][]string)
	config.toolGroups = make(map[string][]string)

	parseDicts(gjson.ParseBytes(configBytes), &config)

	// keys 缺省时回退 Authorization，而非报错——全局解析失败会让 globalConfig 变 nil 并增大
	// fail-closed 风险，故此处只告警不报错。
	if len(config.Keys) == 0 {
		log.Warnf("keys not configured, falling back to %q", defaultAuthHeaderName)
		config.Keys = []string{defaultAuthHeaderName}
	}

	log.Infof("mcp-server-auth global config: %d keys, %d apikeys, %d groups, %d tool_groups",
		len(config.Keys), len(config.apiKeyToUser), len(config.userToGroups), len(config.toolGroups))

	*globalConfig = config
	return nil
}

// parseOverrideRuleConfig 解析路由/域名级授权（grants），继承全局字典。
//
// 重要：本函数【永不返回 error】。在 wrapper.ParseOverrideRawConfig + fail-closed 下，
// 任一规则解析失败都会导致整个 WASM 插件 OnPluginStart 失败 → Envoy 拒绝创建该过滤器
// → 整条过滤器链失效 → 网关上【所有】路由返回 500 NFCF（曾导致整网关瘫痪）。
// 因此即便 globalConfig 缺失/类型异常，也降级为空配置继续，把"拒绝"留到请求阶段处理。
func parseOverrideRuleConfig(configBytes []byte, globalConfig any, ruleConfig *any) error {
	// 用全新 map 构建配置；先深拷贝继承自 global 的字典（避免改到共享的全局 map），
	// 再叠加规则级字典。globalConfig 在 defaultConfigDisable=true 或全局解析失败时是 nil，
	// 此时不能报错（见上方说明），等价于「字典全部来自规则」。
	config := McpAuthConfig{
		apiKeyToUser: make(map[string]string),
		userToGroups: make(map[string][]string),
		toolGroups:   make(map[string][]string),
	}
	if globalConfig != nil {
		if g, ok := globalConfig.(McpAuthConfig); ok {
			config.Keys = append([]string(nil), g.Keys...)
			for k, v := range g.apiKeyToUser {
				config.apiKeyToUser[k] = v
			}
			for k, v := range g.userToGroups {
				config.userToGroups[k] = append([]string(nil), v...)
			}
			for k, v := range g.toolGroups {
				config.toolGroups[k] = append([]string(nil), v...)
			}
		} else {
			log.Errorf("unexpected global config type %T, using rule-level config only", globalConfig)
		}
	}

	// 叠加规则级字典：支持把 keys/user_apikeys/group_users/tool_groups 直接写在 matchRules 里
	// （defaultConfigDisable=true 时字典进不到 global，必须由规则自带）。
	root := gjson.ParseBytes(configBytes)
	parseDicts(root, &config)

	// keys 仍为空则回退 Authorization，保证 extractToken 至少能读一个头。
	if len(config.Keys) == 0 {
		config.Keys = []string{defaultAuthHeaderName}
	}

	// grants 重新构建。
	config.grants = nil

	gjson.GetBytes(configBytes, "grants").ForEach(func(_, g gjson.Result) bool {
		grant := Grant{
			Group:    g.Get("group").String(),
			Apikey:   g.Get("apikey").String(),
			ListOnly: g.Get("list_only").Bool(), // 缺省/非法 → false，向后兼容
		}
		for _, tg := range g.Get("tool_groups").Array() {
			grant.ToolGroups = append(grant.ToolGroups, tg.String())
		}
		if grant.Group == "" && grant.Apikey == "" {
			log.Warnf("grant skipped: neither group nor apikey is set")
			return true
		}
		config.grants = append(config.grants, grant)
		return true
	})

	// 向后兼容：旧 allow:[token...] 等价于把这些 apikey 直授全部工具（tool_groups=["*"]）。
	gjson.GetBytes(configBytes, "allow").ForEach(func(_, t gjson.Result) bool {
		token := t.String()
		if token == "" {
			return true
		}
		config.grants = append(config.grants, Grant{Apikey: token, ToolGroups: []string{wildcard}})
		return true
	})

	// 身份注入与上游认证。解析失败只会得到零值（Enabled=false）——绝不返回 error，
	// 见本函数顶部说明。
	config.identity = parseIdentityInject(root)
	config.upstreamAuth = parseUpstreamAuth(root)

	// Redis 客户端在解析期建立。Init 会返回 error，但【绝不能】往上抛——抛了就是整个
	// 插件 OnPluginStart 失败 → 该 listener 全部更新被拒。建不起来就退化为纯回源。
	if config.identity.Cache.Enabled {
		cc := config.identity.Cache
		client := wrapper.NewRedisClusterClient(wrapper.FQDNCluster{FQDN: cc.ServiceName, Port: cc.ServicePort})
		if err := client.Init("", cc.Password, int64(cc.Timeout), wrapper.WithDataBase(cc.Database)); err != nil {
			log.Warnf("mcp-server-auth: redis init failed, falling back to origin-only: %v", err)
			config.identity.Cache.Enabled = false
		} else {
			config.redisClient = client
		}
	}

	log.Debugf("mcp-server-auth route config: %d grants, identity_inject=%v", len(config.grants), config.identity.Enabled)
	*ruleConfig = config
	return nil
}

// ============================================================================
// 阶段一：身份门禁
// ============================================================================

// onJsonRpcRequest 在每个 JSON-RPC 请求上执行身份解析与 server 访问门禁，
// 并把该调用者被授予的工具集写入 ctx，供后续 tools/call 与 tools/list 使用。
func onJsonRpcRequest(ctx wrapper.HttpContext, cfg any, id utils.JsonRpcID, method string, params gjson.Result, rawBody []byte) types.Action {
	config, ok := cfg.(McpAuthConfig)
	if !ok {
		log.Error("mcp-server-auth: invalid config type")
		return types.ActionContinue
	}
	// headers 阶段判定身份不可用（on_error/on_incomplete = deny）：在此用 JSON-RPC
	// 规范格式拒绝。headers 阶段发裸 403 + text/plain 会让 MCP 客户端显示成无法解析的
	// 传输错误，所以那边只做标记、把拒绝留到这里。
	if ctx.GetContext(ctxIdentityDenied) != nil {
		utils.OnJsonRpcResponseError(ctx, fmt.Errorf("user identity is temporarily unavailable"), utils.ErrInternalError)
		return types.ActionContinue
	}
	// 未绑定任何 grant 的路由（命中全局兜底）：不强制鉴权，放行。
	if len(config.grants) == 0 {
		log.Debug("mcp-server-auth: no grants on this route, skip auth")
		return types.ActionContinue
	}

	listGroups, callGroups, consumer, deny, passed := resolveAndGate(ctx, config)
	if !passed {
		return deny
	}

	ctx.SetContext(ctxAllowedListGroups, listGroups)
	ctx.SetContext(ctxAllowedCallGroups, callGroups)
	// 门禁通过后再确认一次身份头（onRequestHeaders 已注入过，这里覆盖为门禁后的权威值）。
	applyConsumerHeaders(consumer)
	// 注：x-envoy-allow-mcp-tools 的注入已移到 onRequestHeaders（请求头阶段）——body 阶段改请求头传不到
	// 后续 mcp-server host，headers 阶段才可靠。这里不再设。

	log.Infof("mcp-server-auth: request authenticated, consumer=%s, method=%s", consumer, method)
	return types.ActionContinue
}

// onRequestHeaders 请求头阶段：仅凭 api-key（Authorization 头此阶段已可读）解析调用者身份与可见工具集，
// 注入两类请求头：
//   - x-envoy-allow-mcp-tools：供 mcp-server 托管插件在生成 tools/list 时自过滤（OPEN_API 场景生效）。
//   - x-mse-consumer / x-api-key-name：调用方身份，与 ai-route-auth 一致（见 ai-route-auth/main.go
//     Step 4）。必须在请求头阶段注入——cluster-key-rate-limit 等插件只挂 ProcessRequestHeaders，
//     读不到 body 阶段（onJsonRpcRequest）才写的头，否则按调用方限流永远命中不了。
//
// 不在此拒绝——身份门禁仍由 onJsonRpcRequest 在 body 阶段处理（能发规范的 JSON-RPC 错误）。
func onRequestHeaders(ctx wrapper.HttpContext, cfg any) types.Action {
	config, ok := cfg.(McpAuthConfig)
	if !ok {
		clearConsumerHeaders()
		return types.ActionContinue
	}
	// 第一件事：无条件清掉所有受管头。之后才谈解析与注入。
	clearManagedHeaders(config)
	// 先把用户凭证存进 ctx：下面的 defer 会覆盖 Authorization，body 阶段就再也读不到它了。
	if tok, st := extractToken(config.Keys); st == tokenOK {
		ctx.SetContext(ctxRequestToken, tok)
	}
	// 上游固定 Key 用 defer 注入——必须晚于下面读原始 Authorization 的 computeGroups /
	// usernameOf，否则自己把要读的凭证覆盖掉了。挂起路径同样会执行 defer（此时请求只是
	// 挂起、header 仍可写），resume 后一起发出。
	defer applyUpstreamAuth(config.upstreamAuth)

	if len(config.grants) == 0 {
		// 本路由不鉴权：身份无从确认，受管头已清空，不注入任何身份。
		return types.ActionContinue
	}
	// 只算不拒：computeGroups 无副作用，不发任何响应。拒绝仍由 body 阶段 onJsonRpcRequest 处理。
	listGroups, _, consumer, st, matched := computeGroups(ctx, config)
	if st != tokenOK || !matched {
		return types.ActionContinue
	}
	applyConsumerHeaders(consumer)
	applyToolListAllowHeader(config, listGroups)

	// 身份属性注入。返回 true 表示已发起异步调用（Redis / 回源 AUC），必须挂起等回调。
	if resolveIdentity(ctx, config, usernameOf(ctx, config)) {
		return types.HeaderStopAllIterationAndWatermark
	}
	return types.ActionContinue
}

// clearManagedHeaders 无条件移除所有由本插件负责的下游头。
//
// 必须在任何解析之前执行、且与解析结果无关。理由与 clearConsumerHeaders 相同（见其注释），
// 并额外多一条：identity_inject.headers 里的头会被上游当作【可信身份】使用，一旦漏清，
// 任何人带一个伪造的 X-User-Email 就能让上游把别人的邮箱当成调用者身份。
func clearManagedHeaders(cfg McpAuthConfig) {
	clearConsumerHeaders()
	for _, h := range cfg.identity.managedHeaders() {
		_ = proxywasm.RemoveHttpRequestHeader(h)
	}
	// ⚠️ 上游认证头【不能】在这里删。它通常就是 Authorization —— 也就是本插件自己要读的
	// 那个 token 来源。提前删掉会让 extractToken 拿不到凭证，整条路由的认证直接 401
	// "No Key Authentication information found"（曾真实发生）。
	// 客户端凭证不外泄由 applyUpstreamAuth 负责：它 Replace 覆盖同名头，value 为空时才删。
}

// applyUpstreamAuth 注入上游固定认证头。放在插件里而不是用
// higress.io/request-header-control-update 注解，是因为注解值会明文落在 Ingress 上，
// 不满足需求里「通过平台密钥或 Secret 引用保存」。
// 在 onRequestHeaders 的 defer 里调用，因此晚于所有读原始 Authorization 的地方
// （computeGroups / usernameOf），不会把自己要读的凭证提前覆盖掉。
func applyUpstreamAuth(u UpstreamAuth) {
	if u.Header == "" {
		return
	}
	if u.Value == "" {
		// 配了上游认证头但没有值：没东西可注入，但也绝不能把客户端的凭证透传给上游。
		_ = proxywasm.RemoveHttpRequestHeader(u.Header)
		return
	}
	// Replace 本身就覆盖客户端同名头，无需先删。
	_ = proxywasm.ReplaceHttpRequestHeader(u.Header, u.ValuePrefix+u.Value)
}

// usernameOf 复用已解析的 token 反查 username。与 computeGroups 内的口径保持一致。
func usernameOf(ctx wrapper.HttpContext, config McpAuthConfig) string {
	apiKey, st := tokenForRequest(ctx, config.Keys)
	if st != tokenOK {
		return ""
	}
	return config.apiKeyToUser[apiKey]
}

// consumerHeaders 是本插件注入的下游身份头。
// x-mse-consumer 是 higress 的通用 consumer 约定（cluster-key-rate-limit 的 limit_by_consumer /
// limit_by_per_consumer 固定读它）；x-api-key-name 与 ai-route-auth 对齐，让 AI 路由与 MCP 两条链路
// 能共用同一份限流配置。两者取值相同，均为 buildConsumer 的 "user/apikey后8位"。
var consumerHeaders = []string{"x-mse-consumer", "x-api-key-name"}

// applyConsumerHeaders 注入调用方身份。
func applyConsumerHeaders(consumer string) {
	for _, h := range consumerHeaders {
		_ = proxywasm.ReplaceHttpRequestHeader(h, consumer)
	}
}

// clearConsumerHeaders 移除客户端自带的身份头。
//
// 必须清：请求头阶段只算不拒，身份没解析出来时请求仍会继续走到 body 阶段才被门禁拦下，
// 而限流计数发生在请求头阶段——早于门禁。不清的话，任何人带一个伪造的 x-api-key-name
// 就能把计数记到别人的桶上（烧掉受害者额度），或靠轮换该头绕开自己的限额。
func clearConsumerHeaders() {
	for _, h := range consumerHeaders {
		_ = proxywasm.RemoveHttpRequestHeader(h)
	}
}

// applyToolListAllowHeader 在请求阶段设置 x-envoy-allow-mcp-tools=调用者可见工具名(逗号分隔)，
// 供 mcp-server 托管插件生成 tools/list 时自过滤。
//   - listGroups 含 "*"（含发现 key 等通配授权）→ 不设头，host 默认放行全部（看全量）。
//   - 非通配：写 listGroups 覆盖的工具名并集。
//   - 可见集为空 → 写占位名，强制 host 过滤为空（host 把空串当作"未设头=放行全部"，故不能发空串）。
func applyToolListAllowHeader(config McpAuthConfig, listGroups []string) {
	for _, g := range listGroups {
		if g == wildcard {
			return
		}
	}
	visible := make(map[string]struct{})
	for _, g := range listGroups {
		for _, tool := range config.toolGroups[g] {
			visible[tool] = struct{}{}
		}
	}
	names := make([]string, 0, len(visible))
	for tool := range visible {
		names = append(names, tool)
	}
	value := strings.Join(names, ",")
	if value == "" {
		value = "__mcp_auth_no_tool__" // 占位：确保 host 过滤为空而非放行全部
	}
	_ = proxywasm.ReplaceHttpRequestHeader("x-envoy-allow-mcp-tools", value)
}

// onFallbackHttpRequest 对非 JSON-RPC 请求（如 SSE 建链）施加同样的 server 访问门禁。
func onFallbackHttpRequest(ctx wrapper.HttpContext, cfg any, headers [][2]string, body []byte) types.Action {
	config, ok := cfg.(McpAuthConfig)
	if !ok || len(config.grants) == 0 {
		return types.ActionContinue
	}
	_, _, consumer, deny, passed := resolveAndGate(ctx, config)
	if !passed {
		return deny
	}
	applyConsumerHeaders(consumer)
	log.Infof("mcp-server-auth: non-jsonrpc request authenticated, consumer=%s", consumer)
	return types.ActionContinue
}

// resolveAndGate 提取凭证、解析身份并做 server 访问门禁（OR 语义）。
// 返回 (可见工具集, 可调用工具集, 下游 consumer 标识, 拒绝时的 action, 是否通过)。
// 可调用集仅由非 list_only 的命中 grant 贡献；可见集由全部命中 grant 贡献。
// computeGroups 纯计算：提取 token、算出可见/可调用工具集与 consumer，**不发任何响应**。
// 供 headers 阶段（只算不拒）与 body 阶段（据此再决定拒绝）共用，避免在 headers 阶段误发 401/403。
// 返回的 st 为 token 提取状态（tokenOK/tokenMissing/tokenMulti）。
func computeGroups(ctx wrapper.HttpContext, config McpAuthConfig) (listGroups, callGroups []string, consumer string, st int, matched bool) {
	apiKey, tokenSt := tokenForRequest(ctx, config.Keys)
	if tokenSt != tokenOK {
		return nil, nil, "", tokenSt, false
	}

	user := config.apiKeyToUser[apiKey] // apikey 未知时为 ""
	groups := config.userToGroups[user]

	allowedList := make(map[string]struct{})
	allowedCall := make(map[string]struct{})
	for _, g := range config.grants {
		hit := false
		if g.Apikey != "" && g.Apikey == apiKey {
			hit = true
		} else if g.Group != "" && contains(groups, g.Group) {
			hit = true
		}
		if !hit {
			continue
		}
		matched = true // list_only grant 也算命中 → 通过 server 门禁
		for _, tg := range g.ToolGroups {
			allowedList[tg] = struct{}{}
			if !g.ListOnly {
				allowedCall[tg] = struct{}{}
			}
		}
	}

	return setKeys(allowedList), setKeys(allowedCall), buildConsumer(user, apiKey), tokenSt, matched
}

// resolveAndGate 在 computeGroups 基础上做 server 访问门禁：按 tokenState/matched 发拒绝响应（有副作用）。
// 仅供 body 阶段（onJsonRpcRequest / onFallbackHttpRequest）调用——它们确实需要在此拒绝。
func resolveAndGate(ctx wrapper.HttpContext, config McpAuthConfig) (listGroups, callGroups []string, consumer string, deny types.Action, passed bool) {
	listGroups, callGroups, consumer, st, matched := computeGroups(ctx, config)
	switch st {
	case tokenMulti:
		return nil, nil, "", deniedMultiKeyAuthData(), false
	case tokenMissing:
		return nil, nil, "", deniedNoKeyAuthData(), false
	}
	if !matched {
		log.Warnf("mcp-server-auth: credential not authorized to access this mcp server")
		return nil, nil, "", deniedUnauthorizedToken(), false
	}
	return listGroups, callGroups, consumer, types.ActionContinue, true
}

// ============================================================================
// 阶段二：工具调用鉴权
// ============================================================================

// onToolCall 在 tools/call 上校验：调用者被授予的工具集是否覆盖目标工具。
func onToolCall(ctx wrapper.HttpContext, cfg any, toolName string, toolArgs gjson.Result, rawBody []byte) types.Action {
	config, ok := cfg.(McpAuthConfig)
	if !ok {
		return types.ActionContinue
	}
	allowed := toSet(getGroups(ctx, ctxAllowedCallGroups))

	// "*" 工具集：放行全部工具。
	if _, all := allowed[wildcard]; all {
		return types.ActionContinue
	}

	// 工具名 -> 所属工具集；只要其所属任一工具集在被授予集合内即放行。
	for tgName, tools := range config.toolGroups {
		if _, granted := allowed[tgName]; !granted {
			continue
		}
		if contains(tools, toolName) {
			return types.ActionContinue
		}
	}

	log.Warnf("mcp-server-auth: tool %q is not authorized for this consumer", toolName)
	utils.OnJsonRpcResponseError(ctx, fmt.Errorf("tool %q is not authorized", toolName), utils.ErrInvalidRequest)
	return types.ActionContinue
}

// ============================================================================
// 阶段三：tools/list 响应过滤
// ============================================================================

// onToolListResponse 过滤 tools/list 响应，仅保留调用者有权调用的工具。
func onToolListResponse(ctx wrapper.HttpContext, cfg any, tools gjson.Result, rawBody []byte) types.Action {
	config, ok := cfg.(McpAuthConfig)
	if !ok {
		return types.ActionContinue
	}
	allowed := toSet(getGroups(ctx, ctxAllowedListGroups))

	// "*" 工具集：可见全部工具，不改动响应。
	if _, all := allowed[wildcard]; all {
		return types.ActionContinue
	}

	// 计算可见工具名集合 = 被授予工具集覆盖的所有工具名并集。
	visible := make(map[string]struct{})
	for tgName, ts := range config.toolGroups {
		if _, granted := allowed[tgName]; !granted {
			continue
		}
		for _, t := range ts {
			visible[t] = struct{}{}
		}
	}

	filtered := make([]json.RawMessage, 0, len(tools.Array()))
	tools.ForEach(func(_, tool gjson.Result) bool {
		if _, ok := visible[tool.Get("name").String()]; ok {
			filtered = append(filtered, json.RawMessage(tool.Raw))
		}
		return true
	})

	newBody, err := sjson.SetBytes(rawBody, "result.tools", filtered)
	if err != nil {
		log.Errorf("mcp-server-auth: failed to filter tools/list: %v", err)
		return types.ActionContinue
	}
	if err := proxywasm.ReplaceHttpResponseBody(newBody); err != nil {
		log.Errorf("mcp-server-auth: failed to replace tools/list response body: %v", err)
		return types.ActionContinue
	}
	log.Infof("mcp-server-auth: tools/list filtered, %d of %d tools visible", len(filtered), len(tools.Array()))
	return types.ActionContinue
}

// ============================================================================
// 辅助函数
// ============================================================================

// tokenForRequest 取本请求的用户凭证：优先用请求头阶段存进 ctx 的值。
//
// 不能只依赖当场读头：配了上游认证时 Authorization 已被换成上游固定 Key，
// 直接读会让门禁拿错凭证。ctx 里没有(如插件未走请求头阶段)才回退到读头。
func tokenForRequest(ctx wrapper.HttpContext, keys []string) (string, int) {
	if v, ok := ctx.GetContext(ctxRequestToken).(string); ok && v != "" {
		return v, tokenOK
	}
	return extractToken(keys)
}

// extractToken 从配置的请求头中提取唯一 token。
// Authorization 头必须携带 Bearer scheme；其它自定义头取原始值。
func extractToken(keys []string) (string, int) {
	var tokens []string
	for _, key := range keys {
		value, err := proxywasm.GetHttpRequestHeader(key)
		if err != nil || value == "" {
			continue
		}
		if strings.EqualFold(key, "Authorization") {
			token, ok := parseBearerToken(value)
			if !ok {
				log.Warn("Authorization header without a valid Bearer scheme, ignored")
				continue
			}
			tokens = append(tokens, token)
			continue
		}
		tokens = append(tokens, value)
	}
	if len(tokens) > 1 {
		return "", tokenMulti
	}
	if len(tokens) == 0 {
		return "", tokenMissing
	}
	return tokens[0], tokenOK
}

// getGroups 从 ctx 按 key 读取请求阶段写入的工具集列表（可见集或可调用集）。
func getGroups(ctx wrapper.HttpContext, key string) []string {
	if v, ok := ctx.GetContext(key).([]string); ok {
		return v
	}
	return nil
}

// buildConsumer 生成下游 consumer 标识：user/apikey后8位（沿用 ai-route-auth 格式）。
func buildConsumer(user, apiKey string) string {
	suffix := apiKey
	if len(apiKey) > 8 {
		suffix = apiKey[len(apiKey)-8:]
	}
	if user == "" {
		user = "anonymous"
	}
	return user + "/" + suffix
}

// parseBearerToken 解析 `Authorization: Bearer <token>`（scheme 大小写不敏感）。
func parseBearerToken(value string) (string, bool) {
	const prefix = "Bearer "
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		token := strings.TrimSpace(value[len(prefix):])
		if token != "" {
			return token, true
		}
	}
	return "", false
}

func contains(arr []string, item string) bool {
	for _, i := range arr {
		if i == item {
			return true
		}
	}
	return false
}

func toSet(arr []string) map[string]struct{} {
	set := make(map[string]struct{}, len(arr))
	for _, s := range arr {
		set[s] = struct{}{}
	}
	return set
}

// setKeys 把 set 的键收集成切片（无序）。
func setKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ============================================================================
// 拒绝响应
// ============================================================================

func deniedMultiKeyAuthData() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusUnauthorized, "mcp-server-auth.multi_key", WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. Multi Key Authentication information found."), -1)
	return types.ActionContinue
}

func deniedNoKeyAuthData() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusUnauthorized, "mcp-server-auth.no_key", WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. No Key Authentication information found."), -1)
	return types.ActionContinue
}

func deniedUnauthorizedToken() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusForbidden, "mcp-server-auth.unauthorized", WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. Credential is not allowed to access this MCP server."), -1)
	return types.ActionContinue
}

func WWWAuthenticateHeader(realm string) [][2]string {
	return [][2]string{
		{"WWW-Authenticate", fmt.Sprintf("Key realm=%s", realm)},
	}
}
