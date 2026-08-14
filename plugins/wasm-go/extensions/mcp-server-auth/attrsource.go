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
	"net/http"
	"net/url"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

// ctxIdentityDenied body 阶段读取的标记：headers 阶段判定身份不可用（或模板缺值）。
const ctxIdentityDenied = "mcp_auth_identity_denied"

// resolveIdentity 取属性并注入。返回 true 表示已发起异步调用、调用方必须挂起请求。
//
// 分层：VM 缓存 → Redis（见 readThrough） → 回源 AUC。
func resolveIdentity(ctx wrapper.HttpContext, cfg McpAuthConfig, username string) bool {
	if !cfg.identity.Enabled || len(cfg.identity.Headers) == 0 {
		return false
	}
	if username == "" {
		// 身份没解析出来：不注入。门禁本身仍由 body 阶段处理。
		finishIdentity(ctx, cfg, "", nil, false, false)
		return false
	}
	// VM 缓存命中：同步返回，完全不挂起请求。
	if a, ok := localCacheGet(username); ok {
		finishIdentity(ctx, cfg, username, a, true, false)
		return false
	}
	return readThrough(ctx, cfg, username)
}

// fetchFromOrigin 调 AUC 属性接口。返回 true 表示调用已发出。
// done 的第三个参数 absent=true 表示 AUC 明确回答「该用户不存在」（404），
// 调用方据此写负缓存；其它失败一律 absent=false，不能写负缓存（否则把 AUC 的一次抖动
// 放大成一个 TTL 的持续拒绝）。
func fetchFromOrigin(cfg McpAuthConfig, username string, done func(userAttrs, bool, bool)) bool {
	o := cfg.identity.Origin
	if o.ServiceName == "" || o.Path == "" {
		log.Warn("mcp-server-auth: identity origin not configured")
		done(nil, false, false)
		return false
	}
	client := wrapper.NewClusterClient(wrapper.FQDNCluster{
		FQDN: o.ServiceName,
		Host: o.Host,
		Port: o.ServicePort,
	})
	path := o.Path + "?username=" + url.QueryEscape(username)
	headers := [][2]string{{o.TokenHeader, o.Token}}
	err := client.Get(path, headers, func(statusCode int, _ http.Header, body []byte) {
		if statusCode != http.StatusOK {
			// 不打 body：AUC 的错误体可能回显 username。
			log.Warnf("mcp-server-auth: identity origin returned %d", statusCode)
			// 只有 404 算「用户不存在」，可写负缓存；5xx/超时是 AUC 侧故障，不能缓存。
			done(nil, false, statusCode == http.StatusNotFound)
			return
		}
		done(parseAttrs(body), true, false)
	}, o.Timeout)
	if err != nil {
		log.Warnf("mcp-server-auth: identity origin dispatch failed: %v", err)
		done(nil, false, false)
		return false
	}
	return true
}

// attrJSONKeys 白名单字段名 -> AUC GatewayUserAttributesResp 的 JSON key。
var attrJSONKeys = map[string]string{
	"user_id":  "userId",
	"username": "username",
	"name":     "name",
	"email":    "email",
	"mobile":   "mobile",
}

// parseAttrs 把 AUC 响应转成白名单字段表。
func parseAttrs(body []byte) userAttrs {
	r := gjson.ParseBytes(body)
	a := userAttrs{}
	for field, jsonKey := range attrJSONKeys {
		if v := r.Get(jsonKey).String(); v != "" {
			a[field] = v
		}
	}
	return a
}

// applyIdentityHeaders 注入展开后的身份头。返回 false 表示有模板缺值（调用方走 OnIncomplete）。
func applyIdentityHeaders(cfg McpAuthConfig, a userAttrs) bool {
	rendered, complete := cfg.identity.renderHeaders(a)
	for name, value := range rendered {
		_ = proxywasm.ReplaceHttpRequestHeader(name, value)
	}
	return complete
}

// finishIdentity 收口：注入或降级，并在异步路径上恰好 resume 一次。
//
// resume 纪律：paused 为 true 时【每条分支】都必须恰好调用一次 ResumeHttpRequest 或
// 发一次本地响应（后者会终止请求，不需要 resume）。少一次 → 请求挂死到超时；
// 多一次 → 双 resume。
func finishIdentity(ctx wrapper.HttpContext, cfg McpAuthConfig, username string, a userAttrs, ok bool, paused bool) {
	if ok {
		complete := applyIdentityHeaders(cfg, a)
		if complete {
			localCachePut(username, a, cfg.identity.Cache.LocalTTL)
			if paused {
				proxywasm.ResumeHttpRequest()
			}
			return
		}
		// 属性拿到了但模板缺值（如 AUC 里 mobile 为空）。这与「取不到属性」是两回事，
		// 走独立的 OnIncomplete —— 缺省 deny，否则请求会带着缺失的身份头发给上游
		// 而 OnError 根本不触发，对做鉴权的上游是 fail-open。
		if cfg.identity.OnIncomplete == "skip" {
			log.Warn("mcp-server-auth: identity template incomplete, skipping affected headers")
			localCachePut(username, a, cfg.identity.Cache.LocalTTL)
			if paused {
				proxywasm.ResumeHttpRequest()
			}
			return
		}
		log.Warn("mcp-server-auth: identity template incomplete, denying request")
		denyIdentity(ctx, paused)
		return
	}
	if cfg.identity.OnError == "pass" {
		// 放行但不带身份头。受管头已在入口清空，这里什么都不用做。
		log.Warn("mcp-server-auth: identity unavailable, passing through without identity headers")
		if paused {
			proxywasm.ResumeHttpRequest()
		}
		return
	}
	// fail close。必须由插件自己发拒绝响应——nova 建自定义插件时 FailStrategy 写死
	// FAIL_OPEN，指望它会变成静默放行。
	log.Warn("mcp-server-auth: identity unavailable, denying request")
	denyIdentity(ctx, paused)
}

// denyIdentity 按请求形态选拒绝格式，并保证 resume 恰好一次。
// OnError 与 OnIncomplete 两条 deny 路径共用它，避免两处各写一遍走偏。
//
// 拒绝格式分两条路：
//   - 有 body 的 JSON-RPC 请求：不在这里发裸 HTTP 403。MCP 客户端期望 JSON-RPC 错误对象，
//     裸 403 + text/plain 会在客户端显示成无法解析的传输错误（现有门禁正是为此才把拒绝
//     放在 body 阶段用 utils.OnJsonRpcResponseError 发，见 onToolCall）。这里只把
//     「身份不可用」写进 ctx 并 resume，交给 body 阶段按规范格式拒绝。
//   - 无 body 的 SSE 建链（GET /sse）：body 阶段不会执行，只能在此发 HTTP 403。
func denyIdentity(ctx wrapper.HttpContext, paused bool) {
	if paused && ctx.HasRequestBody() {
		ctx.SetContext(ctxIdentityDenied, true)
		proxywasm.ResumeHttpRequest()
		return
	}
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusForbidden, "mcp-server-auth.identity_unavailable",
		WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. User identity is temporarily unavailable."), -1)
}

// ============================================================================
// VM 内一级缓存
// ============================================================================

// localAttrCacheMax VM 内缓存的条目上限。
//
// 必须有上限：wasm VM 生命周期与 gateway pod 同长，只写不清的 map 会随用户数无界增长，
// 而网关内存历来是本集群的故障源（2Gi 时 wasm VM 建不起来会冻结整个 listener）。
// 这层缓存只为挡同一用户的突发连击，几百条足够。
const localAttrCacheMax = 512

// localAttrCache VM 内一级缓存。每个 worker thread 一个 VM、缓存不共享，
// 所以它只用来挡同一用户的突发连击，主命中率靠 Redis。
var localAttrCache = map[string]localAttrEntry{}

type localAttrEntry struct {
	attrs    userAttrs
	expireAt int64 // 秒级 unix 时间
}

func localCacheGet(username string) (userAttrs, bool) {
	e, ok := localAttrCache[username]
	if !ok || nowSeconds() >= e.expireAt {
		return nil, false
	}
	return e.attrs, true
}

func localCachePut(username string, a userAttrs, ttlSec int) {
	if ttlSec <= 0 || username == "" {
		return
	}
	// 到上限先做一次惰性清理（只扫过期项）；清完仍满则整体丢弃重建。
	// 不做 LRU：这层缓存的价值只在几秒内的连击，实现复杂度不值得。
	if len(localAttrCache) >= localAttrCacheMax {
		now := nowSeconds()
		for k, e := range localAttrCache {
			if now >= e.expireAt {
				delete(localAttrCache, k)
			}
		}
		if len(localAttrCache) >= localAttrCacheMax {
			localAttrCache = make(map[string]localAttrEntry, localAttrCacheMax)
		}
	}
	localAttrCache[username] = localAttrEntry{attrs: a, expireAt: nowSeconds() + int64(ttlSec)}
}

// nowSeconds 当前秒级 unix 时间。
//
// 用 time.Now() 是经过确认的：wasm-go wrapper 自己就在用（plugin_wrapper.go 的
// time.Now().UnixMilli()），这条 toolchain 下 WASI clock 可用。
// ⚠️ 不要写成 proxywasm.GetCurrentTimeNanoseconds() —— 该 SDK 版本没有这个导出。
func nowSeconds() int64 {
	return time.Now().Unix()
}

// ============================================================================
// Redis read-through 缓存
// ============================================================================

// cacheKey 缓存键。带 prefix 便于与同实例其它插件的 key 区分。
func cacheKey(c CacheConf, username string) string {
	return c.KeyPrefix + username
}

// absentSentinel 负缓存哨兵。AUC 明确返回「用户不存在」时写它，命中后直接走 OnError，
// 不再打 AUC —— 否则一个已删用户的 apikey 会让客户端每次重试都回源一次。
const absentSentinel = `{"__absent__":true}`

func isAbsentSentinel(raw []byte) bool {
	return gjson.GetBytes(raw, "__absent__").Bool()
}

// readThrough Redis 命中直接用；未命中/出错则回源并写回。
// 返回 true 表示已发起异步调用。
func readThrough(ctx wrapper.HttpContext, cfg McpAuthConfig, username string) bool {
	c := cfg.identity.Cache
	if !c.Enabled || cfg.redisClient == nil {
		return originThenFinish(ctx, cfg, username)
	}
	err := cfg.redisClient.Get(cacheKey(c, username), func(rsp resp.Value) {
		if rsp.Error() == nil && !rsp.IsNull() {
			raw := []byte(rsp.String())
			// 负缓存哨兵：AUC 说过这个用户不存在，别再打 AUC。
			if isAbsentSentinel(raw) {
				log.Debug("mcp-server-auth: negative cache hit, user absent")
				finishIdentity(ctx, cfg, username, nil, false, true)
				return
			}
			a := parseAttrs(raw)
			// 必须校验字段覆盖：缓存 key 全网关共享，而回写只存本路由引用到的字段。
			// 别的路由写的条目可能缺本路由需要的字段，直接用会静默丢 header 且持续整个 ttl。
			if len(a) > 0 && cfg.identity.covers(a) {
				finishIdentity(ctx, cfg, username, a, true, true)
				return
			}
			if len(a) > 0 {
				log.Debug("mcp-server-auth: cached attrs do not cover this route's fields, refetching")
			}
		} else if rsp.Error() != nil {
			// Redis 是软依赖：报错只降级回源，不失败。
			log.Warnf("mcp-server-auth: redis get failed, falling back to origin: %v", rsp.Error())
		}
		fetchAndWriteBack(ctx, cfg, username)
	})
	if err != nil {
		log.Warnf("mcp-server-auth: redis dispatch failed, falling back to origin: %v", err)
		return originThenFinish(ctx, cfg, username)
	}
	return true
}

// originThenFinish 纯回源（无缓存或缓存不可用）。
func originThenFinish(ctx wrapper.HttpContext, cfg McpAuthConfig, username string) bool {
	return fetchFromOrigin(cfg, username, func(a userAttrs, ok bool, absent bool) {
		finishIdentity(ctx, cfg, username, a, ok, true)
	})
}

// fetchAndWriteBack 回源后写回缓存，再收口。已在挂起状态下调用。
func fetchAndWriteBack(ctx wrapper.HttpContext, cfg McpAuthConfig, username string) {
	fetchFromOrigin(cfg, username, func(a userAttrs, ok bool, absent bool) {
		if !ok {
			// 用户不存在 → 写负缓存后再收口；AUC 故障 → 不写缓存，直接收口。
			if absent {
				writeNegative(cfg, username, func() { finishIdentity(ctx, cfg, username, nil, false, true) })
				return
			}
			finishIdentity(ctx, cfg, username, nil, false, true)
			return
		}
		// 回写。在 SETEX 的回调里 resume：多一次内网 RTT，换「resume 恰好一次」的确定性。
		// 先 resume 再 fire-and-forget 会让回调触发时 http context 可能已销毁。
		writeBack(cfg, username, a, func() { finishIdentity(ctx, cfg, username, a, true, true) })
	})
}

// writeBack SETEX 写回。成功与失败都走 next()，保证 resume 恰好一次。
// 只缓存模板实际引用到的字段：Redis 开着 RDB，缓存内容会落盘，字段越少暴露面越小。
func writeBack(cfg McpAuthConfig, username string, a userAttrs, next func()) {
	c := cfg.identity.Cache
	if !c.Enabled || cfg.redisClient == nil {
		next()
		return
	}
	projected := map[string]string{}
	for _, f := range cfg.identity.referencedFields() {
		if v, ok := a[f]; ok {
			projected[attrJSONKeys[f]] = v
		}
	}
	payload, err := json.Marshal(projected)
	if err != nil {
		next()
		return
	}
	setEx(cfg, cacheKey(c, username), string(payload), jitteredTTL(c), next)
}

// writeNegative 写负缓存。negative_ttl<=0 表示关闭该能力。
// 【只对 404 调用】：5xx/超时是 AUC 侧故障，负缓存会把一次抖动放大成一个 TTL 的持续拒绝。
func writeNegative(cfg McpAuthConfig, username string, next func()) {
	c := cfg.identity.Cache
	if !c.Enabled || cfg.redisClient == nil || c.NegativeTTL <= 0 {
		next()
		return
	}
	setEx(cfg, cacheKey(c, username), absentSentinel, c.NegativeTTL, next)
}

// setEx 统一的写回入口：成功与失败都走 next()，保证 resume 恰好一次。
func setEx(cfg McpAuthConfig, key, val string, ttl int, next func()) {
	if err := cfg.redisClient.SetEx(key, val, ttl, func(resp.Value) { next() }); err != nil {
		log.Warnf("mcp-server-auth: redis setex dispatch failed: %v", err)
		next()
	}
}

// jitteredTTL 给 TTL 加抖动，避免同批用户集中过期后一起回源打 AUC。
// 随机源用当前秒即可，不需要密码学强度。
func jitteredTTL(c CacheConf) int {
	if c.TTLJitter <= 0 {
		return c.TTL
	}
	return c.TTL + int(uint32(nowSeconds())%uint32(c.TTLJitter))
}
