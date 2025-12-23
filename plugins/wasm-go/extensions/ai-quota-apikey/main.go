package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-quota-apikey/util"
)

// ============================================================================
// 常量定义
// ============================================================================

const (
	pluginName = "ai-quota-apikey"

	// 默认配置值
	defaultAuthHeaderName  = "Authorization"
	defaultApiKeyQueryName = "api_key"
	bearerPrefix           = "Bearer "

	// Context Keys - 用于在请求上下文中存储数据
	ctxKeyChatMode       = "chatMode"
	ctxKeyAdminMode      = "adminMode"
	ctxKeyApiKey         = "apiKey"
	ctxKeyRuleName       = "ruleName"
	ctxKeyHasQuotaConfig = "hasQuotaConfig"
	ctxKeyPresetQuota    = "presetQuota"    // 预设配额，在请求头阶段获取
	ctxKeyRemainingQuota = "remainingQuota" // 当前剩余配额，在请求头阶段获取
)

// ============================================================================
// 模式枚举
// ============================================================================

// ChatMode 表示请求的聊天模式
type ChatMode string

const (
	ChatModeCompletion ChatMode = "completion" // 聊天完成模式
	ChatModeAdmin      ChatMode = "admin"      // 管理模式
	ChatModeNone       ChatMode = "none"       // 非AI请求
)

// AdminMode 表示管理操作的类型
type AdminMode string

const (
	AdminModeRefresh AdminMode = "refresh" // 刷新配额
	AdminModeList    AdminMode = "list"    // 列出配额
	AdminModeDelete  AdminMode = "delete"  // 删除配额
	AdminModeNone    AdminMode = "none"    // 非管理操作
)

// ============================================================================
// 配置结构体
// ============================================================================

// QuotaConfig 插件配置
type QuotaConfig struct {
	// Redis 配置
	redisInfo   RedisInfo
	redisClient wrapper.RedisClient

	// 配额管理配置
	RedisKeyPrefix string // Redis key 前缀
	AdminApiKey    string // 管理员 API Key
	AdminPath      string // 管理接口路径

	// API Key 提取配置
	ApiKeySource     string // "header" 或 "query"
	ApiKeyHeaderName string // 请求头名称
	ApiKeyQueryName  string // 查询参数名称
	HashApiKey       bool   // 是否对 API Key 进行哈希

	// 路由配置
	RuleName string // 从 matchRules 配置中获取的规则名称
}

// RedisInfo Redis 连接信息
type RedisInfo struct {
	ServiceName string `required:"true" yaml:"service_name" json:"service_name"`
	ServicePort int    `required:"false" yaml:"service_port" json:"service_port"`
	Username    string `required:"false" yaml:"username" json:"username"`
	Password    string `required:"false" yaml:"password" json:"password"`
	Timeout     int    `required:"false" yaml:"timeout" json:"timeout"`
	Database    int    `required:"false" yaml:"database" json:"database"`
}

// ============================================================================
// 插件入口
// ============================================================================

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseOverrideConfig(parseConfig, parseRuleConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody),
	)
}

// ============================================================================
// 配置解析
// ============================================================================

// parseConfig 解析插件配置
func parseConfig(json gjson.Result, config *QuotaConfig) error {
	log.Debugf("parse config()")

	// 管理接口配置
	config.AdminPath = json.Get("admin_path").String()
	config.AdminApiKey = json.Get("admin_api_key").String()
	if config.AdminPath == "" {
		config.AdminPath = "/quota-manager"
	}
	if config.AdminApiKey == "" {
		return errors.New("missing admin_api_key in config")
	}

	// API Key 来源配置
	config.ApiKeySource = json.Get("api_key_source").String()
	if config.ApiKeySource == "" {
		config.ApiKeySource = "header"
	}
	if config.ApiKeySource != "header" && config.ApiKeySource != "query" {
		return errors.New("api_key_source must be 'header' or 'query'")
	}

	config.ApiKeyHeaderName = json.Get("api_key_header_name").String()
	if config.ApiKeyHeaderName == "" {
		config.ApiKeyHeaderName = defaultAuthHeaderName
	}
	config.ApiKeyQueryName = json.Get("api_key_query_name").String()
	if config.ApiKeyQueryName == "" {
		config.ApiKeyQueryName = defaultApiKeyQueryName
	}

	config.HashApiKey = json.Get("hash_api_key").Bool()

	// Redis 配置
	config.RedisKeyPrefix = json.Get("redis_key_prefix").String()
	if config.RedisKeyPrefix == "" {
		config.RedisKeyPrefix = "chat_quota_apikey:"
	}

	redisConfig := json.Get("redis")
	if !redisConfig.Exists() {
		return errors.New("missing redis in config")
	}

	serviceName := redisConfig.Get("service_name").String()
	if serviceName == "" {
		return errors.New("redis service name must not be empty")
	}

	servicePort := int(redisConfig.Get("service_port").Int())
	if servicePort == 0 {
		if strings.HasSuffix(serviceName, ".static") {
			servicePort = 80
		} else {
			servicePort = 6379
		}
	}

	config.redisInfo = RedisInfo{
		ServiceName: serviceName,
		ServicePort: servicePort,
		Username:    redisConfig.Get("username").String(),
		Password:    redisConfig.Get("password").String(),
		Timeout:     int(redisConfig.Get("timeout").Int()),
		Database:    int(redisConfig.Get("database").Int()),
	}
	if config.redisInfo.Timeout == 0 {
		config.redisInfo.Timeout = 1000
	}

	config.redisClient = wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	})

	return config.redisClient.Init(
		config.redisInfo.Username,
		config.redisInfo.Password,
		int64(config.redisInfo.Timeout),
		wrapper.WithDataBase(config.redisInfo.Database),
	)
}

// parseRuleConfig 解析路由规则配置，继承全局配置并覆盖 rule_name
func parseRuleConfig(json gjson.Result, global QuotaConfig, config *QuotaConfig) error {
	*config = global
	if ruleName := json.Get("rule_name").String(); ruleName != "" {
		config.RuleName = ruleName
	}
	return nil
}

// ============================================================================
// API Key 处理
// ============================================================================

// extractApiKey 从请求中提取 API Key
func extractApiKey(ctx wrapper.HttpContext, config QuotaConfig) (string, error) {
	if config.ApiKeySource == "header" {
		headerValue, err := proxywasm.GetHttpRequestHeader(config.ApiKeyHeaderName)
		if err != nil || headerValue == "" {
			return "", errors.New("api key not found in header")
		}
		return extractApiKeyFromHeader(headerValue, config.ApiKeyHeaderName)
	}

	if config.ApiKeySource == "query" {
		rawPath := ctx.Path()
		path, err := url.Parse(rawPath)
		if err != nil {
			return "", fmt.Errorf("failed to parse path: %v", err)
		}
		apiKey := path.Query().Get(config.ApiKeyQueryName)
		if apiKey == "" {
			return "", errors.New("api key not found in query parameter")
		}
		return strings.TrimSpace(apiKey), nil
	}

	return "", errors.New("invalid api_key_source configuration")
}

// extractApiKeyFromHeader 从请求头中提取 API Key
func extractApiKeyFromHeader(headerValue, headerName string) (string, error) {
	if headerName == defaultAuthHeaderName {
		if !strings.HasPrefix(headerValue, bearerPrefix) {
			return "", errors.New("bearer token not found")
		}
		apiKey := strings.TrimSpace(headerValue[len(bearerPrefix):])
		if apiKey == "" {
			return "", errors.New("empty bearer token")
		}
		return apiKey, nil
	}
	apiKey := strings.TrimSpace(headerValue)
	if apiKey == "" {
		return "", errors.New("empty header value")
	}
	return apiKey, nil
}

// hashApiKey 使用 SHA256 对 API Key 进行哈希
func hashApiKey(apiKey string) string {
	hash := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(hash[:])
}

// getApiKeyForHash 获取用于 Hash 存储的 API Key（原始值或哈希值）
func getApiKeyForHash(config QuotaConfig, apiKey string) string {
	if config.HashApiKey {
		return hashApiKey(apiKey)
	}
	return apiKey
}

// ============================================================================
// Redis Key 生成
// ============================================================================

// getRedisKey 生成配额存储的 Redis Key
// 格式: {prefix}_{rule_name}:{api_key} 或 {prefix}{api_key}
func getRedisKey(config QuotaConfig, apiKey string, ruleName ...string) string {
	var route string
	if len(ruleName) > 0 && ruleName[0] != "" {
		route = ruleName[0]
	} else if config.RuleName != "" {
		route = config.RuleName
	}

	keySuffix := apiKey
	if config.HashApiKey {
		keySuffix = hashApiKey(apiKey)
	}

	if route != "" {
		prefix := strings.TrimSuffix(config.RedisKeyPrefix, ":")
		return prefix + "_" + route + ":" + keySuffix
	}
	return config.RedisKeyPrefix + keySuffix
}

// getTotalQuotaKey 生成 Hash 结构的 Redis Key
// 格式: {prefix}_total_quota:{rule_name}
func getTotalQuotaKey(config QuotaConfig, ruleName string) string {
	prefix := strings.TrimSuffix(config.RedisKeyPrefix, ":")
	if ruleName != "" {
		return prefix + "_total_quota:" + ruleName
	}
	return prefix + "_total_quota"
}

// ============================================================================
// 配额值格式化
// ============================================================================

// formatQuotaValue 格式化配额值为字符串
// 格式: "preset:remaining"
func formatQuotaValue(preset, remaining int) string {
	return fmt.Sprintf("%d:%d", preset, remaining)
}

// parseQuotaValue 解析配额值字符串
// 格式: "preset:remaining"
func parseQuotaValue(value string) (preset int, remaining int, err error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid quota value format: %s", value)
	}
	preset, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid preset quota: %s", parts[0])
	}
	remaining, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid remaining quota: %s", parts[1])
	}
	return preset, remaining, nil
}

// ============================================================================
// 请求体解析
// ============================================================================

// parseRequestBody 解析请求体，仅支持 JSON 格式
func parseRequestBody(body string) (map[string]string, error) {
	values := make(map[string]string)

	// 解析为 JSON
	var jsonData map[string]interface{}
	if err := json.Unmarshal([]byte(body), &jsonData); err != nil {
		return nil, fmt.Errorf("request body must be valid JSON: %v", err)
	}

	// JSON 解析成功
	for k, v := range jsonData {
		if str, ok := v.(string); ok {
			values[k] = str
		} else {
			values[k] = fmt.Sprintf("%v", v)
		}
	}

	return values, nil
}

// ============================================================================
// 路由模式判断
// ============================================================================

// getOperationMode 根据请求路径判断操作模式
func getOperationMode(path string, adminPath string) (ChatMode, AdminMode) {
	// 管理接口路径格式: /ai-quota-manager{admin_path}/refresh 或 /ai-quota-manager{admin_path}/list
	// 例如: /ai-quota-manager/quota-manager/refresh
	fullAdminPath := "/ai-quota-manager" + adminPath

	if strings.HasSuffix(path, fullAdminPath+"/refresh") {
		return ChatModeAdmin, AdminModeRefresh
	}
	if strings.HasSuffix(path, fullAdminPath+"/list") {
		return ChatModeAdmin, AdminModeList
	}
	if strings.HasSuffix(path, fullAdminPath+"/delete") {
		return ChatModeAdmin, AdminModeDelete
	}
	// 业务接口路径: /v1/chat/completions
	if strings.HasSuffix(path, "/v1/chat/completions") {
		return ChatModeCompletion, AdminModeNone
	}
	return ChatModeNone, AdminModeNone
}

// ============================================================================
// HTTP 请求处理
// ============================================================================

// onHttpRequestHeaders 处理请求头
func onHttpRequestHeaders(ctx wrapper.HttpContext, config QuotaConfig) types.Action {
	ctx.DisableReroute()
	log.Debugf("onHttpRequestHeaders()")

	rawPath := ctx.Path()
	path, _ := url.Parse(rawPath)
	chatMode, adminMode := getOperationMode(path.Path, config.AdminPath)

	// 非 AI 相关请求，跳过处理
	if chatMode == ChatModeNone {
		return types.ActionContinue
	}

	// 提取 API Key
	apiKey, err := extractApiKey(ctx, config)
	if err != nil || apiKey == "" {
		return deniedNoApiKey()
	}

	// 保存上下文
	ctx.SetContext(ctxKeyChatMode, chatMode)
	ctx.SetContext(ctxKeyAdminMode, adminMode)
	ctx.SetContext(ctxKeyApiKey, apiKey)
	ctx.SetContext(ctxKeyRuleName, config.RuleName)

	log.Debugf("chatMode:%s, adminMode:%s, apiKey:%s", chatMode, adminMode, apiKey)

	// 管理模式处理
	if chatMode == ChatModeAdmin {
		return handleAdminMode(ctx, config, adminMode, apiKey, path)
	}

	// 聊天完成模式：检查配额并预先获取预设配额
	ctx.DontReadRequestBody()
	return checkQuotaAndFetchPreset(ctx, config, apiKey)
}

// handleAdminMode 处理管理模式请求
func handleAdminMode(ctx wrapper.HttpContext, config QuotaConfig, adminMode AdminMode, apiKey string, path *url.URL) types.Action {
	switch adminMode {
	case AdminModeList:
		return listQuotas(ctx, config, apiKey, path)
	case AdminModeRefresh:
		ctx.BufferRequestBody()
		return types.HeaderStopIteration
	case AdminModeDelete:
		ctx.BufferRequestBody()
		return types.HeaderStopIteration
	default:
		return types.ActionContinue
	}
}

// checkQuotaAndFetchPreset 检查配额并获取预设配额（保存到上下文供响应阶段使用）
func checkQuotaAndFetchPreset(ctx wrapper.HttpContext, config QuotaConfig, apiKey string) types.Action {
	redisKey := getRedisKey(config, apiKey)
	ruleName := config.RuleName
	totalQuotaKey := getTotalQuotaKey(config, ruleName)
	apiKeyForHash := getApiKeyForHash(config, apiKey)

	// 第一步：获取当前配额值
	config.redisClient.Get(redisKey, func(response resp.Value) {
		// Key 不存在，跳过配额检查
		if response.IsNull() {
			log.Debugf("apiKey:%s has no quota configured, skipping quota check", apiKey)
			ctx.SetContext(ctxKeyHasQuotaConfig, false)
			proxywasm.ResumeHttpRequest()
			return
		}

		// Redis 错误，允许请求继续
		if err := response.Error(); err != nil {
			log.Warnf("redis error for apiKey:%s: %v, allowing request", apiKey, err)
			ctx.SetContext(ctxKeyHasQuotaConfig, false)
			proxywasm.ResumeHttpRequest()
			return
		}

		// 检查配额是否足够
		remainingQuota := response.Integer()
		if remainingQuota <= 0 {
			log.Debugf("apiKey:%s quota:%d isDenied:true", apiKey, remainingQuota)
			util.SendResponse(http.StatusForbidden, "ai-quota-apikey.noquota", "text/plain", "Request denied by ai quota check, No quota left")
			return
		}

		log.Debugf("apiKey:%s quota:%d isDenied:false", apiKey, remainingQuota)
		ctx.SetContext(ctxKeyHasQuotaConfig, true)
		// 保存当前剩余配额到上下文（用于响应阶段计算新值）
		ctx.SetContext(ctxKeyRemainingQuota, int(remainingQuota))

		// 第二步：获取 Hash 中的预设配额（用于响应阶段更新）
		config.redisClient.HGet(totalQuotaKey, apiKeyForHash, func(hashResponse resp.Value) {
			if err := hashResponse.Error(); err != nil {
				log.Warnf("Failed to get preset quota for apiKey:%s, error:%v", apiKey, err)
				proxywasm.ResumeHttpRequest()
				return
			}

			if !hashResponse.IsNull() {
				quotaValueStr := hashResponse.String()
				presetQuota, _, err := parseQuotaValue(quotaValueStr)
				if err == nil {
					// 保存预设配额到上下文，供响应阶段使用
					ctx.SetContext(ctxKeyPresetQuota, presetQuota)
					log.Debugf("apiKey:%s presetQuota:%d remainingQuota:%d saved to context", apiKey, presetQuota, remainingQuota)
				}
			}
			proxywasm.ResumeHttpRequest()
		})
	})

	return types.HeaderStopAllIterationAndWatermark
}

// onHttpRequestBody 处理请求体
func onHttpRequestBody(ctx wrapper.HttpContext, config QuotaConfig, body []byte) types.Action {
	log.Debugf("onHttpRequestBody()")

	chatMode, ok := ctx.GetContext(ctxKeyChatMode).(ChatMode)
	if !ok || chatMode != ChatModeAdmin {
		return types.ActionContinue
	}

	adminMode, ok := ctx.GetContext(ctxKeyAdminMode).(AdminMode)
	if !ok {
		return types.ActionContinue
	}

	adminApiKey, ok := ctx.GetContext(ctxKeyApiKey).(string)
	if !ok {
		return types.ActionContinue
	}

	if adminMode == AdminModeRefresh {
		return refreshQuota(ctx, config, adminApiKey, string(body))
	}
	if adminMode == AdminModeDelete {
		return deleteQuota(ctx, config, adminApiKey, string(body))
	}

	return types.ActionContinue
}

// ============================================================================
// HTTP 响应处理
// ============================================================================

// onHttpStreamingResponseBody 处理流式响应体
func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config QuotaConfig, data []byte, endOfStream bool) []byte {
	chatMode, ok := ctx.GetContext(ctxKeyChatMode).(ChatMode)
	if !ok || chatMode != ChatModeCompletion {
		return data
	}

	// 记录 Token 使用量
	if usage := tokenusage.GetTokenUsage(ctx, data); usage.TotalToken > 0 {
		ctx.SetContext(tokenusage.CtxKeyInputToken, usage.InputToken)
		ctx.SetContext(tokenusage.CtxKeyOutputToken, usage.OutputToken)
	}

	// 只在流结束时处理
	if !endOfStream {
		return data
	}

	// 检查是否有配额配置
	hasQuotaConfig, ok := ctx.GetContext(ctxKeyHasQuotaConfig).(bool)
	if !ok || !hasQuotaConfig {
		log.Debugf("apiKey has no quota configured, skipping quota decrement")
		return data
	}

	// 获取必要的上下文数据
	inputToken := ctx.GetContext(tokenusage.CtxKeyInputToken)
	outputToken := ctx.GetContext(tokenusage.CtxKeyOutputToken)
	apiKey := ctx.GetContext(ctxKeyApiKey)

	if inputToken == nil || outputToken == nil || apiKey == nil {
		return data
	}

	// 执行配额更新
	updateQuotaOnConsumption(ctx, config,
		apiKey.(string),
		ctx.GetContext(ctxKeyRuleName).(string),
		int(inputToken.(int64)+outputToken.(int64)),
	)

	return data
}

// updateQuotaOnConsumption 在配额消耗时更新 Redis
// 策略：完全使用 fire-and-forget 方式更新单个 Key 和 Hash
// 注意：在 Proxy-WASM 的 onHttpStreamingResponseBody 中，Redis 回调不会执行
// 因此所有值都从上下文中获取（在请求头阶段已保存）
func updateQuotaOnConsumption(ctx wrapper.HttpContext, config QuotaConfig, apiKey, ruleName string, totalToken int) {
	redisKey := getRedisKey(config, apiKey, ruleName)
	totalQuotaKey := getTotalQuotaKey(config, ruleName)
	apiKeyForHash := getApiKeyForHash(config, apiKey)

	log.Debugf("updateQuota: apiKey=%s, ruleName=%s, totalToken=%d", apiKey, ruleName, totalToken)

	// 1. 更新单个配额 Key（fire-and-forget，与 ai-quota 插件一致）
	config.redisClient.DecrBy(redisKey, totalToken, nil)

	// 2. 更新 Hash 中的剩余配额
	// 从上下文获取预设配额和当前剩余配额（在请求头阶段已保存）
	presetQuota, hasPreset := ctx.GetContext(ctxKeyPresetQuota).(int)
	if !hasPreset {
		log.Debugf("No preset quota in context, skipping hash update")
		return
	}

	currentRemainingQuota, hasRemaining := ctx.GetContext(ctxKeyRemainingQuota).(int)
	if !hasRemaining {
		log.Debugf("No remaining quota in context, skipping hash update")
		return
	}

	// 计算新的剩余配额
	newRemainingQuota := currentRemainingQuota - totalToken
	if newRemainingQuota < 0 {
		newRemainingQuota = 0
	}

	// 更新 Hash（fire-and-forget）
	newQuotaValue := formatQuotaValue(presetQuota, newRemainingQuota)
	log.Debugf("Updating hash: key=%s, field=%s, value=%s (preset=%d, remaining=%d->%d, consumed=%d)",
		totalQuotaKey, apiKeyForHash, newQuotaValue, presetQuota, currentRemainingQuota, newRemainingQuota, totalToken)
	config.redisClient.HSet(totalQuotaKey, apiKeyForHash, newQuotaValue, nil)
}

// ============================================================================
// 管理接口实现
// ============================================================================

// refreshQuota 刷新配额
func refreshQuota(ctx wrapper.HttpContext, config QuotaConfig, adminApiKey string, body string) types.Action {
	// 验证管理员 API Key
	if adminApiKey != config.AdminApiKey {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied. Unauthorized admin API Key.")
		return types.ActionContinue
	}

	// 解析请求参数（仅支持 JSON 格式）
	values, err := parseRequestBody(body)
	if err != nil {
		util.SendResponse(http.StatusBadRequest, "ai-quota-apikey.bad_request", "application/json", fmt.Sprintf(`{"error":"%s"}`, err.Error()))
		return types.ActionContinue
	}

	queryApiKey := values["api_key"]
	quota, err := strconv.Atoi(values["quota"])
	if queryApiKey == "" || err != nil {
		util.SendResponse(http.StatusBadRequest, "ai-quota-apikey.bad_request", "application/json", `{"error":"api_key can't be empty and quota must be integer"}`)
		return types.ActionContinue
	}

	ruleName := values["rule_name"]
	if ruleName == "" {
		ruleName = config.RuleName
	}

	// 生成 Redis Keys
	redisKey := getRedisKey(config, queryApiKey, ruleName)
	totalQuotaKey := getTotalQuotaKey(config, ruleName)
	apiKeyForHash := getApiKeyForHash(config, queryApiKey)
	quotaValue := formatQuotaValue(quota, quota)

	// 更新 Redis
	err2 := config.redisClient.Set(redisKey, quota, func(response resp.Value) {
		if err := response.Error(); err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "application/json", fmt.Sprintf(`{"error":"redis error: %v"}`, err))
			return
		}

		log.Debugf("Redis set key=%s quota=%d", redisKey, quota)

		// 更新 Hash
		err := config.redisClient.HSet(totalQuotaKey, apiKeyForHash, quotaValue, func(hashResponse resp.Value) {
			if err := hashResponse.Error(); err != nil {
				log.Warnf("Failed to update hash quota for apiKey:%s, error:%v", queryApiKey, err)
			} else {
				log.Debugf("Updated hash quota for apiKey:%s, value:%s", queryApiKey, quotaValue)
			}
		})
		if err != nil {
			log.Warnf("Failed to call HSet for apiKey:%s, error:%v", queryApiKey, err)
		}

		util.SendResponse(http.StatusOK, "ai-quota-apikey.refreshquota", "application/json", `{"message":"refresh quota successful"}`)
	})

	if err2 != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "application/json", fmt.Sprintf(`{"error":"redis error: %v"}`, err2))
		return types.ActionContinue
	}

	return types.ActionPause
}

// listQuotas 列出配额
func listQuotas(ctx wrapper.HttpContext, config QuotaConfig, adminApiKey string, reqURL *url.URL) types.Action {
	// 验证管理员 API Key
	if adminApiKey != config.AdminApiKey {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied. Unauthorized admin API Key.")
		return types.ActionContinue
	}

	// 获取路由名称
	ruleName := reqURL.Query().Get("rule_name")
	if ruleName == "" {
		ruleName = config.RuleName
	}

	totalQuotaKey := getTotalQuotaKey(config, ruleName)

	err := config.redisClient.HGetAll(totalQuotaKey, func(response resp.Value) {
		if err := response.Error(); err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return
		}

		quotas := buildQuotaList(response, ruleName)

		body, err := json.Marshal(quotas)
		if err != nil {
			util.SendResponse(http.StatusInternalServerError, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("failed to marshal response: %v", err))
			return
		}

		util.SendResponse(http.StatusOK, "ai-quota-apikey.listquotas", "application/json", string(body))
	})

	if err != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
		return types.ActionContinue
	}

	return types.ActionPause
}

// deleteQuota 删除配额
func deleteQuota(ctx wrapper.HttpContext, config QuotaConfig, adminApiKey string, body string) types.Action {
	// 验证管理员 API Key
	if adminApiKey != config.AdminApiKey {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied. Unauthorized admin API Key.")
		return types.ActionContinue
	}

	// 解析请求参数（仅支持 JSON 格式）
	values, err := parseRequestBody(body)
	if err != nil {
		util.SendResponse(http.StatusBadRequest, "ai-quota-apikey.bad_request", "application/json", fmt.Sprintf(`{"error":"%s"}`, err.Error()))
		return types.ActionContinue
	}

	queryApiKey := values["api_key"]
	if queryApiKey == "" {
		util.SendResponse(http.StatusBadRequest, "ai-quota-apikey.bad_request", "application/json", `{"error":"api_key can't be empty"}`)
		return types.ActionContinue
	}

	ruleName := values["rule_name"]
	if ruleName == "" {
		ruleName = config.RuleName
	}

	// 生成 Redis Keys
	redisKey := getRedisKey(config, queryApiKey, ruleName)
	totalQuotaKey := getTotalQuotaKey(config, ruleName)
	apiKeyForHash := getApiKeyForHash(config, queryApiKey)

	// 先删除单个配额 Key（即使 key 不存在也返回成功）
	err = config.redisClient.Del(redisKey, func(response resp.Value) {
		if respErr := response.Error(); respErr != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "application/json", fmt.Sprintf(`{"error":"redis error when deleting key: %v"}`, respErr))
			return
		}

		// Redis Del 命令即使 key 不存在也会返回成功（返回删除的数量，可能是 0）
		deletedCount := response.Integer()
		log.Debugf("Redis deleted key=%s, deleted count=%d", redisKey, deletedCount)

		// 删除 Hash 中的 field（即使 field 不存在也返回成功）
		hdelErr := config.redisClient.HDel(totalQuotaKey, []string{apiKeyForHash}, func(hashResponse resp.Value) {
			if hashErr := hashResponse.Error(); hashErr != nil {
				log.Warnf("Failed to delete hash field for apiKey:%s, error:%v", queryApiKey, hashErr)
				// 即使 Hash 删除失败，也返回成功
				util.SendResponse(http.StatusOK, "ai-quota-apikey.deletequota", "application/json", `{"message":"delete quota successful"}`)
				return
			}

			// HDel 返回删除的 field 数量，即使 field 不存在也会返回 0，但不报错
			deletedFields := hashResponse.Integer()
			log.Debugf("Deleted hash field for apiKey:%s, field:%s, deleted fields=%d", queryApiKey, apiKeyForHash, deletedFields)
			util.SendResponse(http.StatusOK, "ai-quota-apikey.deletequota", "application/json", `{"message":"delete quota successful"}`)
		})
		if hdelErr != nil {
			log.Warnf("Failed to call HDel for apiKey:%s, error:%v", queryApiKey, hdelErr)
			// 即使 HDel 调用失败，也返回成功
			util.SendResponse(http.StatusOK, "ai-quota-apikey.deletequota", "application/json", `{"message":"delete quota successful"}`)
			return
		}
	})

	if err != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "application/json", fmt.Sprintf(`{"error":"redis error: %v"}`, err))
		return types.ActionContinue
	}

	return types.ActionPause
}

// buildQuotaList 从 Redis HGetAll 响应构建配额列表
func buildQuotaList(response resp.Value, ruleName string) []map[string]interface{} {
	if response.IsNull() {
		return []map[string]interface{}{}
	}

	arr := response.Array()
	quotas := make([]map[string]interface{}, 0, len(arr)/2)

	for i := 0; i < len(arr); i += 2 {
		if i+1 >= len(arr) {
			break
		}

		apiKeyField := arr[i].String()
		quotaValueStr := arr[i+1].String()

		presetQuota, remainingQuota, err := parseQuotaValue(quotaValueStr)
		if err != nil {
			log.Warnf("Failed to parse quota value for apiKey:%s, value:%s, error:%v", apiKeyField, quotaValueStr, err)
			continue
		}

		quotaInfo := map[string]interface{}{
			"api_key":         apiKeyField,
			"preset_quota":    presetQuota,
			"remaining_quota": remainingQuota,
		}
		if ruleName != "" {
			quotaInfo["rule_name"] = ruleName
		}
		quotas = append(quotas, quotaInfo)
	}

	return quotas
}

// ============================================================================
// 错误响应
// ============================================================================

// deniedNoApiKey 返回未找到 API Key 的错误响应
func deniedNoApiKey() types.Action {
	util.SendResponse(http.StatusUnauthorized, "ai-quota-apikey.no_key", "text/plain", "Request denied. No API Key found.")
	return types.ActionContinue
}
