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

// ext-header-enrich 在请求转发给上游之前，把调用方的用户属性（userId / username /
// name / email / mobile）以 x-hgw-* 请求头注入，供上游业务系统识别真实用户。
//
// 身份不是自己解析的：直接复用 ai-route-auth / mcp-server-auth 认证通过后写入的
// x-mse-consumer（格式 <username>/<apikey后8位>），取斜杠前半段作为 username，
// 再按 username 查 Redis 缓存 / 回源 AUC 拿属性。
//
// 【强制前置依赖】本插件必须与 ai-route-auth 或 mcp-server-auth 挂在同一条路由上、
// 且排在其后执行。x-mse-consumer 在本插件眼里是可信输入，而它只是一个普通请求头：
// 单独挂载本插件，等于把用户邮箱、手机号做成匿名可读。挂了认证插件时才安全 ——
// ai-route-auth 认证失败直接拒绝，mcp-server-auth 的 clearConsumerHeaders() 会在
// 身份没解析出来时清掉客户端自带的该头。
package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

// ============================================================================
// Constants
// ============================================================================

const (
	pluginName = "ext-header-enrich"

	// managedHeaderPrefix 本插件独占的请求头命名空间。请求头阶段第一件事就是把整个
	// 命名空间清空 —— 不是只清本次要注入的那几个，而是按前缀全清。配置里删掉一条
	// 映射、或不同路由注入的字段集不同时，都不会留下漏网的伪造头。
	managedHeaderPrefix = "x-hgw-"

	// consumerHeader 上游认证插件写入的身份头，取值固定为 <username>/<apikey后8位>
	// （ai-route-auth main.go 的 Step 4、mcp-server-auth 的 applyConsumerHeaders）。
	// 它是本插件的【输入】，因此不在 managedHeaderPrefix 的清理范围内。
	consumerHeader = "x-mse-consumer"

	// gatewayTokenHeader 与 AUC 约定的共享密钥头。AUC 侧密钥来自环境变量
	// GATEWAY_ATTR_TOKEN，服务端未配置密钥时一律拒绝（fail-close）。
	gatewayTokenHeader = "x-gateway-token"

	// negativeMarker AUC 回 404（用户不存在/已禁用）时写进缓存的标记。
	// 取一个不可能与属性 JSON 混淆、且在 redis-cli 里肉眼可读的固定串。
	negativeMarker = "__ext_header_enrich_absent__"

	// 默认值
	defaultAUCPath          = "/v1/user/gateway-attributes"
	defaultAUCTimeout       = 2000
	defaultRedisTimeout     = 2000
	defaultCacheTTL         = 300
	defaultNegativeCacheTTL = 30
	defaultCacheKeyPrefix   = "ext_header_enrich"
)

// ============================================================================
// Plugin Entry Points
// ============================================================================

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseOverrideConfig(parseConfig, parseRuleConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

// ============================================================================
// Configuration Types
// ============================================================================

// AUCSettings AUC 回源的连接与协议参数。
type AUCSettings struct {
	ServiceName string
	ServicePort int64
	Path        string
	Token       string
	Timeout     uint32
}

// RedisSettings 缓存连接参数，与 ai-quota-apikey 同构。
type RedisSettings struct {
	ServiceName string
	ServicePort int64
	Username    string
	Password    string
	Database    int
	Timeout     int64
}

// Settings 是插件配置里【可被 matchRule 字段级覆盖】的那部分，全部为纯数据，
// 便于单测（见 parseSettings / overrideSettings）。
type Settings struct {
	AUC   AUCSettings
	Redis RedisSettings

	CacheTTL         int
	NegativeCacheTTL int
	CacheKeyPrefix   string

	// DenyOnError 对应配置项 on_error：false=allow（默认，放行但不注入任何
	// x-hgw-* 头），true=deny（返回 503）。
	DenyOnError bool
}

// Config 是运行时配置：Settings + 由它构建出来的客户端 + 两个状态位。
type Config struct {
	Settings

	// valid 为 false 表示这份配置没通过校验。此时【不返回解析错误】，而是在请求阶段
	// 走 on_error —— 因为 wasm-go 下任意一段配置解析失败都会让整个插件配置加载失败，
	// 配合 failStrategy=FAIL_CLOSE 会把网关上所有路由一起打挂（ai-route-auth 的
	// parseRuleConfig 注释记录过同一个坑）。
	valid         bool
	invalidReason string

	// ruleConfigured 表示这份配置来自命中的 matchRule 而非 defaultConfig。
	// 未命中 matchRule 的路由只做 x-hgw-* 清理，不做富化。
	ruleConfigured bool

	aucClient   wrapper.HttpClient
	redisClient wrapper.RedisClient
}

// dependencyWarned 让「前置依赖」告警在每个 VM 里只打一次。wasm 插件单线程，
// 一个普通 bool 足够。
var dependencyWarned bool

func warnDependencyOnce() {
	if dependencyWarned {
		return
	}
	dependencyWarned = true
	log.Warnf("%s 依赖上游认证插件（ai-route-auth / mcp-server-auth）产出并清洗 %s；"+
		"若本插件单独挂载在一条没有认证插件的路由上，任何客户端都能自带 %s 冒充身份，"+
		"从而匿名读到用户邮箱与手机号", pluginName, consumerHeader, consumerHeader)
}

// ============================================================================
// Configuration Parsing
// ============================================================================

// parseConfig 解析 defaultConfig。
//
// 配置格式见 wasmplugin.yaml / README.md。注意本函数【永不返回 error】，理由见
// Config.valid 的注释。
func parseConfig(json gjson.Result, config *Config) error {
	warnDependencyOnce()

	settings, err := parseSettings(json)
	config.Settings = settings
	if err != nil {
		config.valid = false
		config.invalidReason = err.Error()
		log.Errorf("invalid defaultConfig: %v（该路由将按 on_error 处理，不注入任何 %s* 头）",
			err, managedHeaderPrefix)
		return nil
	}

	config.valid = true
	buildAUCClient(config)
	if err := buildRedisClient(config); err != nil {
		// Redis 建不起来不是致命错误：缓存挂了不该让功能挂，所有请求直接回源 AUC。
		log.Errorf("failed to init redis client: %v（将全部回源 AUC）", err)
	}
	log.Infof("config loaded: auc=%s:%d%s redis=%s:%d cache_ttl=%ds negative_cache_ttl=%ds on_error=%s",
		settings.AUC.ServiceName, settings.AUC.ServicePort, settings.AUC.Path,
		settings.Redis.ServiceName, settings.Redis.ServicePort,
		settings.CacheTTL, settings.NegativeCacheTTL, onErrorName(settings.DenyOnError))
	return nil
}

// parseRuleConfig 解析 matchRule 的 config，语义是【字段级覆盖】而非整体替换：
// 只写要改的那几个字段即可，`config: {}` 表示完全继承 defaultConfig。
//
// 与 parseConfig 一样永不返回 error。
func parseRuleConfig(json gjson.Result, global Config, config *Config) error {
	*config = global
	config.ruleConfigured = true

	settings, aucChanged, redisChanged, err := overrideSettings(json, global.Settings)
	config.Settings = settings
	if err != nil {
		config.valid = false
		config.invalidReason = err.Error()
		log.Errorf("invalid matchRule config: %v（该路由将按 on_error 处理，不注入任何 %s* 头）",
			err, managedHeaderPrefix)
		return nil
	}
	config.valid = true
	config.invalidReason = ""

	// 只有连接参数变了才重建客户端 —— 大多数 matchRule 只改 on_error 之类的字段，
	// 重建一遍等于每条路由多占一个 Redis 连接池。
	if aucChanged || global.aucClient == nil {
		buildAUCClient(config)
	}
	if redisChanged || global.redisClient == nil {
		if err := buildRedisClient(config); err != nil {
			log.Errorf("failed to init redis client for rule: %v（该路由将全部回源 AUC）", err)
		}
	}
	return nil
}

func buildAUCClient(config *Config) {
	config.aucClient = wrapper.NewClusterClient(wrapper.FQDNCluster{
		FQDN: config.AUC.ServiceName,
		Port: config.AUC.ServicePort,
	})
}

func buildRedisClient(config *Config) error {
	client := wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: config.Redis.ServiceName,
		Port: config.Redis.ServicePort,
	})
	config.redisClient = client
	return client.Init(
		config.Redis.Username,
		config.Redis.Password,
		config.Redis.Timeout,
		wrapper.WithDataBase(config.Redis.Database),
	)
}

// parseSettings 解析一份完整配置：填默认值 + 必填校验。用于 defaultConfig。
func parseSettings(json gjson.Result) (Settings, error) {
	var s Settings
	if err := applySettings(json, &s); err != nil {
		return s, err
	}
	fillDefaults(&s)
	return s, validateSettings(s)
}

// overrideSettings 在 base 之上做字段级覆盖，用于 matchRule。
//
// 返回的两个 bool 分别表示 AUC / Redis 的【连接参数】是否发生变化 —— 只有变了才
// 需要重建对应的客户端。path / token / timeout 是每次调用时才用的参数，不影响
// AUC 客户端本身，因此不计入 aucChanged。
func overrideSettings(json gjson.Result, base Settings) (Settings, bool, bool, error) {
	s := base
	if err := applySettings(json, &s); err != nil {
		return s, false, false, err
	}
	fillDefaults(&s)

	aucChanged := s.AUC.ServiceName != base.AUC.ServiceName || s.AUC.ServicePort != base.AUC.ServicePort
	redisChanged := s.Redis != base.Redis

	return s, aucChanged, redisChanged, validateSettings(s)
}

// applySettings 把 json 里【出现了的】字段写进 s，没出现的保持原值。
func applySettings(json gjson.Result, s *Settings) error {
	if auc := json.Get("auc"); auc.Exists() {
		if v := auc.Get("service_name"); v.Exists() {
			s.AUC.ServiceName = strings.TrimSpace(v.String())
		}
		if v := auc.Get("service_port"); v.Exists() {
			s.AUC.ServicePort = v.Int()
		}
		if v := auc.Get("path"); v.Exists() {
			s.AUC.Path = strings.TrimSpace(v.String())
		}
		if v := auc.Get("gateway_token"); v.Exists() {
			s.AUC.Token = v.String()
		}
		if v := auc.Get("timeout"); v.Exists() {
			if v.Int() <= 0 {
				return fmt.Errorf("auc.timeout must be positive, got %d", v.Int())
			}
			s.AUC.Timeout = uint32(v.Int())
		}
	}

	if r := json.Get("redis"); r.Exists() {
		if v := r.Get("service_name"); v.Exists() {
			s.Redis.ServiceName = strings.TrimSpace(v.String())
		}
		if v := r.Get("service_port"); v.Exists() {
			s.Redis.ServicePort = v.Int()
		}
		if v := r.Get("username"); v.Exists() {
			s.Redis.Username = v.String()
		}
		if v := r.Get("password"); v.Exists() {
			s.Redis.Password = v.String()
		}
		if v := r.Get("database"); v.Exists() {
			s.Redis.Database = int(v.Int())
		}
		if v := r.Get("timeout"); v.Exists() {
			if v.Int() <= 0 {
				return fmt.Errorf("redis.timeout must be positive, got %d", v.Int())
			}
			s.Redis.Timeout = v.Int()
		}
	}

	if v := json.Get("cache_ttl"); v.Exists() {
		if v.Int() <= 0 {
			return fmt.Errorf("cache_ttl must be positive, got %d", v.Int())
		}
		s.CacheTTL = int(v.Int())
	}
	// negative_cache_ttl: 0 是合法值，语义为「关闭 negative cache」。
	if v := json.Get("negative_cache_ttl"); v.Exists() {
		if v.Int() < 0 {
			return fmt.Errorf("negative_cache_ttl must not be negative, got %d", v.Int())
		}
		s.NegativeCacheTTL = int(v.Int())
	}
	if v := json.Get("cache_key_prefix"); v.Exists() {
		s.CacheKeyPrefix = strings.TrimSpace(v.String())
	}
	if v := json.Get("on_error"); v.Exists() {
		switch strings.ToLower(strings.TrimSpace(v.String())) {
		case "allow":
			s.DenyOnError = false
		case "deny":
			s.DenyOnError = true
		default:
			return fmt.Errorf("on_error must be \"allow\" or \"deny\", got %q", v.String())
		}
	}
	return nil
}

// fillDefaults 只填零值，不覆盖已配置的值。
//
// negative_cache_ttl 的 0 无法与「没配」区分，这里的取舍是：要关掉 negative cache
// 就显式写在 defaultConfig 里（那份配置会整体作为 base 传给 matchRule），而不是靠
// matchRule 写 0 —— 后者会被这里重新填回 30。README 里写明了这一点。
func fillDefaults(s *Settings) {
	if s.AUC.Path == "" {
		s.AUC.Path = defaultAUCPath
	}
	if s.AUC.Timeout == 0 {
		s.AUC.Timeout = defaultAUCTimeout
	}
	if s.Redis.Timeout == 0 {
		s.Redis.Timeout = defaultRedisTimeout
	}
	if s.CacheTTL == 0 {
		s.CacheTTL = defaultCacheTTL
	}
	if s.NegativeCacheTTL == 0 {
		s.NegativeCacheTTL = defaultNegativeCacheTTL
	}
	if s.CacheKeyPrefix == "" {
		s.CacheKeyPrefix = defaultCacheKeyPrefix
	}
}

func validateSettings(s Settings) error {
	if s.AUC.ServiceName == "" {
		return errors.New("auc.service_name is required")
	}
	if s.AUC.ServicePort <= 0 {
		return fmt.Errorf("auc.service_port must be positive, got %d", s.AUC.ServicePort)
	}
	// 密钥缺失必须报错而不是发一个没带密钥的请求：AUC 侧是 fail-close，会一律拒绝，
	// 表现为「插件没生效」而不是「配置漏了」，极难排查。
	if s.AUC.Token == "" {
		return errors.New("auc.gateway_token is required")
	}
	if s.Redis.ServiceName == "" {
		return errors.New("redis.service_name is required")
	}
	if s.Redis.ServicePort <= 0 {
		return fmt.Errorf("redis.service_port must be positive, got %d", s.Redis.ServicePort)
	}
	return nil
}

func onErrorName(deny bool) string {
	if deny {
		return "deny"
	}
	return "allow"
}

// ============================================================================
// Request Processing
// ============================================================================

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	// Step 1：无条件清理整个 x-hgw- 命名空间。不可配、不可跳过、且在任何分支之前 ——
	// 下游把这些头当可信输入，不清的话任何人自带一个 x-hgw-username: admin 就能冒充。
	stripManagedHeaders()

	// Step 2：本插件不看 body。不加会触发 ai-quota-apikey 踩过的大 body 413。
	ctx.DontReadRequestBody()

	// Step 3：没命中 matchRule 的路由只做清理，不做富化（也不算错误）。
	if !config.ruleConfigured {
		log.Debugf("no matched rule, %s* stripped only", managedHeaderPrefix)
		return types.ActionContinue
	}
	if !config.valid {
		log.Errorf("config unusable: %s", config.invalidReason)
		return errorAction(config, "invalid config")
	}

	// Step 4：从上游认证插件写入的 x-mse-consumer 里切出 username。
	username, ok := resolveUsername(proxywasm.GetHttpRequestHeader)
	if !ok {
		log.Warnf("no usable identity in %s, skipping enrichment", consumerHeader)
		return errorAction(config, "missing identity")
	}
	key := cacheKey(config.CacheKeyPrefix, username)

	// Step 5：查缓存。Redis 不可用不是错误，直接回源。
	if config.redisClient == nil || !config.redisClient.Ready() {
		log.Warnf("redis not ready, falling back to AUC for user %q", username)
		return fetchFromAUC(config, username, key)
	}

	err := config.redisClient.Get(key, func(response resp.Value) {
		switch {
		case response.Error() != nil:
			log.Warnf("redis get %s failed: %v, falling back to AUC", key, response.Error())
			resumeViaAUC(config, username, key)
		case response.IsNull():
			log.Debugf("cache miss for user %q", username)
			resumeViaAUC(config, username, key)
		case response.String() == negativeMarker:
			// negative 命中：AUC 刚说过这个用户不存在，不再回源。
			log.Debugf("negative cache hit for user %q", username)
			resumeOnError(config, "user not found (cached)")
		default:
			headers, err := buildUserHeaders([]byte(response.String()))
			if err != nil {
				// 缓存内容损坏也不算致命错误，回源重建。
				log.Warnf("corrupted cache at %s: %v, falling back to AUC", key, err)
				resumeViaAUC(config, username, key)
				return
			}
			injectHeaders(headers)
			log.Debugf("cache hit for user %q", username)
			proxywasm.ResumeHttpRequest()
		}
	})
	if err != nil {
		log.Warnf("failed to dispatch redis get: %v, falling back to AUC", err)
		return fetchFromAUC(config, username, key)
	}

	// 必须是 HeaderStopAllIterationAndWatermark(4) 而不是 HeaderStopIteration(1)：
	// 后者只停 header filter 的迭代，请求仍会继续走下去，等回调回来时头已经发给上游了，
	// 注入不生效。
	return types.HeaderStopAllIterationAndWatermark
}

// fetchFromAUC 是请求头阶段（同步）的回源入口，返回值直接作为 filter 的 Action。
func fetchFromAUC(config Config, username, key string) types.Action {
	if dispatchAUC(config, username, key) {
		return types.HeaderStopAllIterationAndWatermark
	}
	return errorAction(config, "auc dispatch failed")
}

// resumeViaAUC 是回调里（已挂起）的回源入口，自己负责恢复或拒绝请求。
func resumeViaAUC(config Config, username, key string) {
	if !dispatchAUC(config, username, key) {
		resumeOnError(config, "auc dispatch failed")
	}
}

// dispatchAUC 发起一次 AUC 回源。返回 false 表示请求根本没发出去 —— 此时不会有任何
// 回调，调用方必须自己收尾。
func dispatchAUC(config Config, username, key string) bool {
	if config.aucClient == nil {
		log.Errorf("auc client is not initialized")
		return false
	}
	rawURL := fmt.Sprintf("%s?username=%s", config.AUC.Path, url.QueryEscape(username))
	headers := [][2]string{
		{gatewayTokenHeader, config.AUC.Token},
		{"Accept", "application/json"},
	}

	err := config.aucClient.Get(rawURL, headers, func(statusCode int, _ http.Header, body []byte) {
		switch {
		case statusCode >= 200 && statusCode < 300:
			attrs, err := buildUserHeaders(body)
			if err != nil {
				// 响应体解析不出任何属性，不写缓存 —— 缓存一份垃圾只会让问题持续一个 TTL。
				log.Errorf("unusable auc response for user %q: %v", username, err)
				resumeOnError(config, "unusable auc response")
				return
			}
			injectHeaders(attrs)
			writeCache(config, key, string(body), config.CacheTTL)
			log.Debugf("enriched user %q from auc", username)
			proxywasm.ResumeHttpRequest()

		case statusCode == http.StatusNotFound:
			// 只有 404 写 negative 缓存：它是「这个用户确实不存在」这一稳定事实。
			// 一把仍在流通、但对应用户已销号的 api-key，不缓存的话每个请求都回源一次。
			log.Warnf("auc has no gateway identity for user %q", username)
			writeCache(config, key, negativeMarker, config.NegativeCacheTTL)
			resumeOnError(config, "user not found")

		default:
			// 5xx / 超时 / 连接失败一律不写缓存：缓存后端故障会让 AUC 恢复之后
			// 网关还要多扛一个 TTL 才恢复正常。
			log.Errorf("auc call failed for user %q: status=%d body=%s",
				username, statusCode, truncate(string(body), 256))
			resumeOnError(config, "auc call failed")
		}
	}, config.AUC.Timeout)

	if err != nil {
		log.Errorf("failed to dispatch auc call for user %q: %v", username, err)
		return false
	}
	return true
}

// writeCache 回写缓存。失败只记日志 —— 缓存写不进去不影响本次请求已经完成的富化。
func writeCache(config Config, key, value string, ttl int) {
	if ttl <= 0 {
		// negative_cache_ttl: 0 表示关闭 negative cache。
		return
	}
	if config.redisClient == nil || !config.redisClient.Ready() {
		return
	}
	if err := config.redisClient.SetEx(key, value, ttl, nil); err != nil {
		log.Warnf("failed to write cache %s: %v", key, err)
	}
}

// errorAction 是请求头阶段（同步）的 on_error 收敛点。
func errorAction(config Config, reason string) types.Action {
	if config.DenyOnError {
		return denyRequest(reason)
	}
	// allow：放行，但不注入任何 x-hgw-* 头。Step 1 的清理已经执行，下游看到的是
	// 「无身份」而不是「伪造身份」。
	log.Debugf("on_error=allow, request continues without %s* headers (%s)", managedHeaderPrefix, reason)
	return types.ActionContinue
}

// resumeOnError 是回调里（已挂起）的 on_error 收敛点。
func resumeOnError(config Config, reason string) {
	if config.DenyOnError {
		denyRequest(reason)
		return
	}
	log.Debugf("on_error=allow, request continues without %s* headers (%s)", managedHeaderPrefix, reason)
	proxywasm.ResumeHttpRequest()
}

func denyRequest(reason string) types.Action {
	log.Warnf("on_error=deny, rejecting request (%s)", reason)
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusServiceUnavailable,
		pluginName+".enrich_failed",
		[][2]string{{"Content-Type", "application/json"}},
		// 对外不暴露 reason：区分「用户不存在」与「AUC 挂了」会变成一个账号探测口子。
		[]byte(`{"error":"failed to resolve caller identity attributes"}`),
		-1,
	)
	return types.ActionContinue
}

// ============================================================================
// Header 处理（薄壳）
// ============================================================================

// stripManagedHeaders 按前缀清空 x-hgw-* 命名空间。
func stripManagedHeaders() {
	headers, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		log.Criticalf("failed to read request headers, cannot strip %s* namespace: %v",
			managedHeaderPrefix, err)
		return
	}
	for _, h := range headers {
		if !isManagedHeader(h[0]) {
			continue
		}
		if err := proxywasm.RemoveHttpRequestHeader(h[0]); err != nil {
			log.Criticalf("failed to remove client-supplied header %s: %v", h[0], err)
			continue
		}
		log.Warnf("removed client-supplied header %s (this namespace is gateway-owned)", h[0])
	}
}

// injectHeaders 按固定顺序注入，日志与抓包可读性更好。
func injectHeaders(attrs map[string]string) {
	for _, a := range attributeHeaders {
		v, ok := attrs[a.header]
		if !ok {
			continue
		}
		if err := proxywasm.ReplaceHttpRequestHeader(a.header, v); err != nil {
			log.Errorf("failed to set %s: %v", a.header, err)
		}
	}
}

// ============================================================================
// 纯函数（单测覆盖）
// ============================================================================

// headerGetter 抽出「读请求头」这一步，便于单测注入。生产实现即
// proxywasm.GetHttpRequestHeader —— 缺失的 header 返回 error 而不是空串。
type headerGetter func(string) (string, error)

// resolveUsername 从 x-mse-consumer 切出 username。
//
// 取值形如 "zhangsan/1a2b3c4d"，取第一个 '/' 之前的部分。头缺失、为空、或切出来的
// username 为空（例如 "/1a2b3c4d"）都返回 false，由调用方走 on_error。
func resolveUsername(get headerGetter) (string, bool) {
	raw, err := get(consumerHeader)
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false
	}
	if i := strings.Index(value, "/"); i >= 0 {
		value = value[:i]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

// attributeHeaders 是 AUC 属性字段到注入头名的固定映射。头名按 x-hgw-<字段名小写>
// 机械生成，不可配 —— 字段就固定这五个，没有可配的余地也就不需要配。
var attributeHeaders = []struct {
	field  string
	header string
	// escape 为 true 时对值做 percent-encode。只有 name（中文姓名）需要：HTTP header
	// 值按 RFC 7230 是 ASCII 域，非 ASCII 字节属于已废弃的 obs-text，下游框架、日志
	// 系统、中间代理的处理各凭运气。
	//
	// 必须用 url.PathEscape 而不是 url.QueryEscape：两者对中文结果相同，但空格前者编成
	// %20、后者编成 '+'，而下游最常用的 decodeURIComponent 不认 '+'，会把它当字面加号
	// 留下（"张 三" -> "张+三"）。
	escape bool
}{
	{"userId", "x-hgw-userid", false},
	{"username", "x-hgw-username", false},
	{"name", "x-hgw-name", true},
	{"email", "x-hgw-email", false},
	{"mobile", "x-hgw-mobile", false},
}

// buildUserHeaders 把 AUC 的属性 JSON 变成待注入的头集合。
//
// AUC 未返回或返回空串的字段不注入对应的头（而不是注入一个空头）—— 下游拿到空 header
// 比拿不到更难判断。一个字段都没有时返回 error，这条路径同时用于「缓存内容损坏 -> 回源」
// 的判定。
func buildUserHeaders(body []byte) (map[string]string, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("response is not valid json")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, errors.New("response is not a json object")
	}

	out := make(map[string]string, len(attributeHeaders))
	for _, a := range attributeHeaders {
		value := strings.TrimSpace(root.Get(a.field).String())
		if value == "" {
			continue
		}
		if a.escape {
			value = url.PathEscape(value)
			out[a.header] = value
			continue
		}
		// 明文字段里混进控制字符就是一个 header 注入口子（值来自 AUC 的库表，
		// 而 email / username 是用户可影响的）。宁可少注入一个头，也不把它写进去。
		if !isSafeHeaderValue(value) {
			continue
		}
		out[a.header] = value
	}
	if len(out) == 0 {
		return nil, errors.New("no gateway attribute present in response")
	}
	return out, nil
}

// isSafeHeaderValue 拒绝控制字符（含 CR/LF/NUL 与 DEL）。非 ASCII 字节放行 ——
// 需要编码的 name 已经单独 escape 过了。
func isSafeHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

// cacheKey 用 username 而非 api-key 作 key：同一个人的多把 key 共享一份缓存，
// 且属性本来就按人变化。
func cacheKey(prefix, username string) string {
	return strings.TrimSuffix(prefix, ":") + ":" + username
}

// isManagedHeader 判定一个头名是否落在本插件独占的 x-hgw-* 命名空间里。
func isManagedHeader(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), managedHeaderPrefix)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
