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
	"strings"

	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/tidwall/gjson"
)

// whitelistFields 允许在 Header 模板里引用的字段，白名单之外的 {xxx} 一律不展开。
// 与 AUC GatewayUserAttributesResp 的字段一一对应。
var whitelistFields = []string{"user_id", "username", "name", "email", "mobile"}

// 默认值集中放这里：wasm 里「零值超时」等价于回调可能永不触发 → 请求挂死，
// 所以每个超时都必须有非零默认。
const (
	defaultOriginTimeout uint32 = 500
	defaultCacheTimeout  uint32 = 1000
	defaultCacheTTL             = 300
	defaultCacheKeyPrefix       = "mcp:uattr:"
	// defaultCacheDatabase 刻意不用 0：db0 已被 ai-quota 的配额数据占用且是 noeviction，
	// 缓存写爆会连带打挂配额扣减。
	defaultCacheDatabase = 1
)

// reservedHeaderNames 不允许作为身份 Header 名的保留名（小写）。
//
// 为什么要黑名单：Headers 是管理员自由输入的，插件直接 ReplaceHttpRequestHeader。
//   - x-mse-consumer / x-api-key-name：被 cluster-key-rate-limit 的 limit_by_consumer
//     读取，覆盖它会把限流计数记到别人的桶上；
//   - x-envoy-allow-mcp-tools：mcp-server 托管插件据此过滤 tools/list，覆盖它 = 过滤失效；
//   - 与 upstream_auth.header 同名：会覆盖上游固定 Key，且顺序上必然是身份头赢——
//     applyUpstreamAuth 挂在 defer（函数返回时执行），身份头在异步回调里注入，晚于 defer。
var reservedHeaderNames = map[string]struct{}{
	"x-mse-consumer":          {},
	"x-api-key-name":          {},
	"x-envoy-allow-mcp-tools": {},
}

// CacheConf 属性缓存配置。
type CacheConf struct {
	Enabled     bool
	ServiceName string
	ServicePort int64
	Password    string
	Database    int
	Timeout     uint32
	KeyPrefix   string
	TTL         int
	// TTLJitter 写回时 TTL 随机 +[0,TTLJitter)，避免热点用户集中过期后一起回源打 AUC。
	TTLJitter int
	// NegativeTTL 「用户不存在」的负缓存 TTL，0=关闭。
	NegativeTTL int
	LocalTTL    int
}

// OriginConf 回源 AUC 属性接口的配置。
type OriginConf struct {
	ServiceName string
	ServicePort int64
	Host        string
	Path        string
	TokenHeader string
	Token       string
	Timeout     uint32
}

// IdentityInject 用户身份 Header 注入配置。Enabled 缺省 false。
type IdentityInject struct {
	Enabled bool
	// Headers header 名(小写) -> 值模板。解析期已剔除保留名冲突项。
	Headers map[string]string
	Cache   CacheConf
	Origin  OriginConf
	// OnError 取不到属性（AUC 故障/超时/非 200）时的行为：deny(默认) | pass
	OnError string
	// OnIncomplete 取到属性但模板变量缺值（如 AUC 里 mobile 为空）时的行为：deny(默认) | skip
	//
	// 必须与 OnError 分开：属性拿到了、只是字段为空，这种情况若沿用「跳过该 header」
	// 就会让请求带着缺失的身份头发给上游，而 OnError 根本不触发 —— 对做鉴权的上游是
	// fail-open。缺省 deny 与 OnError 同口径。
	OnIncomplete string
}

// UpstreamAuth 上游固定认证头。Header 存小写。
type UpstreamAuth struct {
	Header      string
	ValuePrefix string
	Value       string
}

// parseIdentityInject 解析 identity_inject。永不返回 error —— 见 parseOverrideRuleConfig
// 顶部的说明，任何解析失败只能降级为「该路由不注入」。
func parseIdentityInject(root gjson.Result) IdentityInject {
	n := root.Get("identity_inject")
	c := IdentityInject{Headers: map[string]string{}, OnError: "deny", OnIncomplete: "deny"}
	if !n.Exists() {
		return c
	}
	c.Enabled = n.Get("enabled").Bool()
	// 非法值一律落到 deny（fail close），不能落到放行。
	if n.Get("on_error").String() == "pass" {
		c.OnError = "pass"
	}
	if n.Get("on_incomplete").String() == "skip" {
		c.OnIncomplete = "skip"
	}
	// upstream_auth.header 也进保留名集合：同名会覆盖上游固定 Key。
	upstreamHeader := strings.ToLower(strings.TrimSpace(root.Get("upstream_auth.header").String()))
	n.Get("headers").ForEach(func(k, v gjson.Result) bool {
		name := strings.ToLower(strings.TrimSpace(k.String()))
		if name == "" {
			return true
		}
		if _, reserved := reservedHeaderNames[name]; reserved {
			log.Warnf("mcp-server-auth: identity header %q is reserved by the gateway, dropped", name)
			return true
		}
		if upstreamHeader != "" && name == upstreamHeader {
			log.Warnf("mcp-server-auth: identity header %q collides with upstream_auth.header, dropped", name)
			return true
		}
		c.Headers[name] = v.String()
		return true
	})

	o := n.Get("origin")
	c.Origin = OriginConf{
		ServiceName: o.Get("service_name").String(),
		ServicePort: o.Get("service_port").Int(),
		Host:        o.Get("host").String(),
		Path:        o.Get("path").String(),
		TokenHeader: o.Get("token_header").String(),
		Token:       o.Get("token").String(),
		Timeout:     uint32(o.Get("timeout").Uint()),
	}
	if c.Origin.Timeout == 0 {
		c.Origin.Timeout = defaultOriginTimeout
	}
	if c.Origin.TokenHeader == "" {
		c.Origin.TokenHeader = "x-gateway-token"
	}
	// 开了开关却没配回源地址 = 配置错误，不是运行时故障。
	//
	// 必须降级为「本路由不注入」而不是走 on_error：后者缺省 deny，会让一次误操作
	// （页面上打开开关、而下发方没补 origin）把整条路由的所有请求全部拒掉。
	// 与 parseOverrideRuleConfig「非法配置只降级、不报错」的一贯口径一致。
	if c.Enabled && (c.Origin.ServiceName == "" || c.Origin.Path == "") {
		log.Errorf("mcp-server-auth: identity_inject enabled but origin not configured (service_name=%q path=%q), injection disabled for this route",
			c.Origin.ServiceName, c.Origin.Path)
		c.Enabled = false
	}

	// cache 段缺省 → 纯回源。这让「higress 自带 redis 未开启」从阻塞项变成性能项。
	if r := n.Get("cache.redis"); r.Exists() && r.Get("service_name").String() != "" {
		c.Cache = CacheConf{
			Enabled:     true,
			ServiceName: r.Get("service_name").String(),
			ServicePort: r.Get("service_port").Int(),
			Password:    r.Get("password").String(),
			Database:    defaultCacheDatabase,
			Timeout:     uint32(r.Get("timeout").Uint()),
			KeyPrefix:   n.Get("cache.key_prefix").String(),
			TTL:         int(n.Get("cache.ttl").Int()),
			TTLJitter:   int(n.Get("cache.ttl_jitter").Int()),
			NegativeTTL: int(n.Get("cache.negative_ttl").Int()),
			LocalTTL:    int(n.Get("cache.local_ttl").Int()),
		}
		if r.Get("database").Exists() {
			c.Cache.Database = int(r.Get("database").Int())
		}
		if c.Cache.ServicePort == 0 {
			c.Cache.ServicePort = 6379
		}
		if c.Cache.Timeout == 0 {
			c.Cache.Timeout = defaultCacheTimeout
		}
		if c.Cache.KeyPrefix == "" {
			c.Cache.KeyPrefix = defaultCacheKeyPrefix
		}
		if c.Cache.TTL <= 0 {
			c.Cache.TTL = defaultCacheTTL
		}
	}
	return c
}

// parseUpstreamAuth 解析 upstream_auth（A 项：上游固定认证头）。
func parseUpstreamAuth(root gjson.Result) UpstreamAuth {
	n := root.Get("upstream_auth")
	return UpstreamAuth{
		Header:      strings.ToLower(strings.TrimSpace(n.Get("header").String())),
		ValuePrefix: n.Get("value_prefix").String(),
		Value:       n.Get("value").String(),
	}
}

// managedHeaders 本插件负责「先删再注」的身份头名集合。
func (c IdentityInject) managedHeaders() []string {
	out := make([]string, 0, len(c.Headers))
	for name := range c.Headers {
		out = append(out, name)
	}
	return out
}

// referencedFields 模板实际引用到的白名单字段。
// 只取这些字段做投影，是为了减少 Redis 里（会被 RDB 落盘的）PII 字段数。
func (c IdentityInject) referencedFields() []string {
	seen := map[string]struct{}{}
	for _, tpl := range c.Headers {
		for _, f := range whitelistFields {
			if strings.Contains(tpl, "{"+f+"}") {
				seen[f] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	return out
}

// userAttrs 键为白名单字段名。
type userAttrs map[string]string

// expandTemplate 展开模板。第二个返回值为 false 表示模板里有变量取不到值。
//
// 缺值时【不】注入该 header，而不是注入空串：上游若按「头存在」判断身份，空串会被当作
// 一个合法的空身份；而白名单之外的变量原样透传给上游更糟（形如 {password} 的字面量）。
func expandTemplate(tpl string, a userAttrs) (string, bool) {
	if !strings.ContainsRune(tpl, '{') {
		return tpl, true
	}
	complete := true
	out := tpl
	for _, f := range whitelistFields {
		ph := "{" + f + "}"
		if !strings.Contains(out, ph) {
			continue
		}
		v, ok := a[f]
		if !ok || v == "" {
			complete = false
			continue
		}
		out = strings.ReplaceAll(out, ph, v)
	}
	// 仍残留 "{...}"：引用了白名单之外的变量，或上面标记了缺值。
	if strings.ContainsRune(out, '{') {
		return out, false
	}
	return out, complete
}

// renderHeaders 展开全部模板。
// 第二个返回值 complete=false 表示至少有一个模板因缺值没能展开——调用方据此走 OnIncomplete，
// 不能只是静默跳过（否则请求会带着缺失的身份头发给上游，而 OnError 不触发 = fail-open）。
func (c IdentityInject) renderHeaders(a userAttrs) (map[string]string, bool) {
	out := make(map[string]string, len(c.Headers))
	complete := true
	for name, tpl := range c.Headers {
		if v, ok := expandTemplate(tpl, a); ok {
			out[name] = v
		} else {
			complete = false
			// 只报 header 名与模板，不报值——值里就是 email/mobile。
			log.Warnf("mcp-server-auth: header %q template %q not fully resolved", name, tpl)
		}
	}
	return out, complete
}

// covers 判断这份属性是否覆盖本路由模板引用到的全部字段。
//
// 用途见 readThrough：缓存 key 是 mcp:uattr:<username>、全网关共享，而回写只存本路由
// 引用到的字段（为减少 RDB 落盘的 PII）。路由 A 只引用 {email} 时写入的条目没有 mobile，
// 路由 B 引用 {mobile} 命中它就会静默丢 header 且持续整个 ttl。所以命中后必须校验覆盖。
func (c IdentityInject) covers(a userAttrs) bool {
	for _, f := range c.referencedFields() {
		if v, ok := a[f]; !ok || v == "" {
			return false
		}
	}
	return true
}
