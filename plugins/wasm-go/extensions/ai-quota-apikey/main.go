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

const (
	pluginName = "ai-quota-apikey"

	// Default values
	defaultAuthHeaderName  = "Authorization"
	defaultApiKeyQueryName = "api_key"
	bearerPrefix           = "Bearer "
)

type ChatMode string

const (
	ChatModeCompletion ChatMode = "completion"
	ChatModeAdmin      ChatMode = "admin"
	ChatModeNone       ChatMode = "none"
)

type AdminMode string

const (
	AdminModeRefresh AdminMode = "refresh"
	AdminModeQuery   AdminMode = "query"
	AdminModeDelta   AdminMode = "delta"
	AdminModeNone    AdminMode = "none"
)

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

type QuotaConfig struct {
	redisInfo        RedisInfo
	RedisKeyPrefix   string
	AdminApiKey      string
	AdminPath        string
	ApiKeySource     string // "header" | "query"
	ApiKeyHeaderName string
	ApiKeyQueryName  string
	HashApiKey       bool
	RouteName        string // route_name from matchRules config
	redisClient      wrapper.RedisClient
}

type RedisInfo struct {
	ServiceName string `required:"true" yaml:"service_name" json:"service_name"`
	ServicePort int    `required:"false" yaml:"service_port" json:"service_port"`
	Username    string `required:"false" yaml:"username" json:"username"`
	Password    string `required:"false" yaml:"password" json:"password"`
	Timeout     int    `required:"false" yaml:"timeout" json:"timeout"`
	Database    int    `required:"false" yaml:"database" json:"database"`
}

func parseConfig(json gjson.Result, config *QuotaConfig) error {
	log.Debugf("parse config()")
	// admin
	config.AdminPath = json.Get("admin_path").String()
	config.AdminApiKey = json.Get("admin_api_key").String()
	if config.AdminPath == "" {
		config.AdminPath = "/quota-manager"
	}
	if config.AdminApiKey == "" {
		return errors.New("missing admin_api_key in config")
	}
	// API-Key source configuration
	config.ApiKeySource = json.Get("api_key_source").String()
	if config.ApiKeySource == "" {
		config.ApiKeySource = "header" // default to header
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
	// Redis
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
			// use default logic port which is 80 for static service
			servicePort = 80
		} else {
			servicePort = 6379
		}
	}
	username := redisConfig.Get("username").String()
	password := redisConfig.Get("password").String()
	timeout := int(redisConfig.Get("timeout").Int())
	if timeout == 0 {
		timeout = 1000
	}
	database := int(redisConfig.Get("database").Int())
	config.redisInfo.ServiceName = serviceName
	config.redisInfo.ServicePort = servicePort
	config.redisInfo.Username = username
	config.redisInfo.Password = password
	config.redisInfo.Timeout = timeout
	config.redisInfo.Database = database
	config.redisClient = wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	})

	return config.redisClient.Init(username, password, int64(timeout), wrapper.WithDataBase(database))
}

// parseRuleConfig parses matchRules config, only route_name is needed, other configs inherit from defaultConfig
func parseRuleConfig(json gjson.Result, global QuotaConfig, config *QuotaConfig) error {
	// Copy all fields from global config
	*config = global

	// Only override route_name if present in matchRules config
	if routeName := json.Get("route_name").String(); routeName != "" {
		config.RouteName = routeName
	}

	return nil
}

// extractApiKey extracts the API key from the request
func extractApiKey(ctx wrapper.HttpContext, config QuotaConfig) (string, error) {
	if config.ApiKeySource == "header" {
		// Try header first
		headerValue, err := proxywasm.GetHttpRequestHeader(config.ApiKeyHeaderName)
		if err == nil && headerValue != "" {
			return extractApiKeyFromHeader(headerValue, config.ApiKeyHeaderName)
		}
		// If header source is configured but not found, return error
		return "", errors.New("api key not found in header")
	} else if config.ApiKeySource == "query" {
		// Try query parameter
		rawPath := ctx.Path()
		path, err := url.Parse(rawPath)
		if err != nil {
			return "", fmt.Errorf("failed to parse path: %v", err)
		}
		queryValues := path.Query()
		apiKey := queryValues.Get(config.ApiKeyQueryName)
		if apiKey == "" {
			return "", errors.New("api key not found in query parameter")
		}
		return strings.TrimSpace(apiKey), nil
	}
	return "", errors.New("invalid api_key_source configuration")
}

// extractApiKeyFromHeader extracts API key from header value
func extractApiKeyFromHeader(headerValue, headerName string) (string, error) {
	if headerName == defaultAuthHeaderName {
		// Authorization header: expect "Bearer <token>" format
		if !strings.HasPrefix(headerValue, bearerPrefix) {
			return "", errors.New("bearer token not found")
		}
		apiKey := strings.TrimSpace(headerValue[len(bearerPrefix):])
		if apiKey == "" {
			return "", errors.New("empty bearer token")
		}
		return apiKey, nil
	}
	// Other headers: use value directly
	apiKey := strings.TrimSpace(headerValue)
	if apiKey == "" {
		return "", errors.New("empty header value")
	}
	return apiKey, nil
}

// hashApiKey hashes the API key using SHA256
func hashApiKey(apiKey string) string {
	hash := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(hash[:])
}

// getRedisKey returns the Redis key for the given API key
// Format: {redis_key_prefix}_{route_name}:{api_key} or {redis_key_prefix}_{route_name}:{hash(api_key)}
func getRedisKey(config QuotaConfig, apiKey string, routeName ...string) string {
	var routeNameStr string
	if len(routeName) > 0 && routeName[0] != "" {
		routeNameStr = routeName[0]
	} else if config.RouteName != "" {
		routeNameStr = config.RouteName
	}

	var keySuffix string
	if config.HashApiKey {
		keySuffix = hashApiKey(apiKey)
	} else {
		keySuffix = apiKey
	}

	if routeNameStr != "" {
		// Format: {redis_key_prefix}_{route_name}:{api_key}
		// Remove trailing colon from prefix if exists, then add underscore and route_name
		prefix := strings.TrimSuffix(config.RedisKeyPrefix, ":")
		return prefix + "_" + routeNameStr + ":" + keySuffix
	}
	return config.RedisKeyPrefix + keySuffix
}

func onHttpRequestHeaders(context wrapper.HttpContext, config QuotaConfig) types.Action {
	context.DisableReroute()
	log.Debugf("onHttpRequestHeaders()")

	rawPath := context.Path()
	path, _ := url.Parse(rawPath)
	chatMode, adminMode := getOperationMode(path.Path, config.AdminPath)

	// If not an AI-related path, skip processing
	if chatMode == ChatModeNone {
		return types.ActionContinue
	}

	// Extract API-Key only for AI-related paths
	apiKey, err := extractApiKey(context, config)
	if err != nil {
		return deniedNoApiKey()
	}
	if apiKey == "" {
		return deniedNoApiKey()
	}

	context.SetContext("chatMode", chatMode)
	context.SetContext("adminMode", adminMode)
	context.SetContext("apiKey", apiKey)
	log.Debugf("chatMode:%s, adminMode:%s, apiKey:%s", chatMode, adminMode, apiKey)
	if chatMode == ChatModeAdmin {
		// query quota
		if adminMode == AdminModeQuery {
			return queryQuota(context, config, apiKey, path)
		}
		if adminMode == AdminModeRefresh || adminMode == AdminModeDelta {
			context.BufferRequestBody()
			return types.HeaderStopIteration
		}
		return types.ActionContinue
	}

	// there is no need to read request body when it is on chat completion mode
	context.DontReadRequestBody()
	// check quota here
	redisKey := getRedisKey(config, apiKey)
	context.SetContext("routeName", config.RouteName)
	config.redisClient.Get(redisKey, func(response resp.Value) {
		// If Redis key doesn't exist (IsNull), skip quota check and allow the request
		if response.IsNull() {
			log.Debugf("apiKey:%s has no quota configured, skipping quota check", apiKey)
			// Mark that this API key has no quota configured, so we won't decrement quota later
			context.SetContext("hasQuotaConfig", false)
			proxywasm.ResumeHttpRequest()
			return
		}
		// If Redis error occurs, log warning but allow the request to avoid service disruption
		if err := response.Error(); err != nil {
			log.Warnf("redis error for apiKey:%s: %v, allowing request", apiKey, err)
			// On error, assume no quota config to avoid decrementing
			context.SetContext("hasQuotaConfig", false)
			proxywasm.ResumeHttpRequest()
			return
		}
		// Mark that this API key has quota configured
		context.SetContext("hasQuotaConfig", true)
		// Check quota only if it's configured and <= 0
		quota := response.Integer()
		if quota <= 0 {
			log.Debugf("apiKey:%s quota:%d isDenied:true", apiKey, quota)
			util.SendResponse(http.StatusForbidden, "ai-quota-apikey.noquota", "text/plain", "Request denied by ai quota check, No quota left")
			return
		}
		log.Debugf("apiKey:%s quota:%d isDenied:false", apiKey, quota)
		proxywasm.ResumeHttpRequest()
	})
	return types.HeaderStopAllIterationAndWatermark
}

func onHttpRequestBody(ctx wrapper.HttpContext, config QuotaConfig, body []byte) types.Action {
	log.Debugf("onHttpRequestBody()")
	chatMode, ok := ctx.GetContext("chatMode").(ChatMode)
	if !ok {
		return types.ActionContinue
	}
	if chatMode == ChatModeNone || chatMode == ChatModeCompletion {
		return types.ActionContinue
	}
	adminMode, ok := ctx.GetContext("adminMode").(AdminMode)
	if !ok {
		return types.ActionContinue
	}
	adminApiKey, ok := ctx.GetContext("apiKey").(string)
	if !ok {
		return types.ActionContinue
	}

	if adminMode == AdminModeRefresh {
		return refreshQuota(ctx, config, adminApiKey, string(body))
	}
	if adminMode == AdminModeDelta {
		return deltaQuota(ctx, config, adminApiKey, string(body))
	}

	return types.ActionContinue
}

func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config QuotaConfig, data []byte, endOfStream bool) []byte {
	chatMode, ok := ctx.GetContext("chatMode").(ChatMode)
	if !ok {
		return data
	}
	if chatMode == ChatModeNone || chatMode == ChatModeAdmin {
		return data
	}
	if usage := tokenusage.GetTokenUsage(ctx, data); usage.TotalToken > 0 {
		ctx.SetContext(tokenusage.CtxKeyInputToken, usage.InputToken)
		ctx.SetContext(tokenusage.CtxKeyOutputToken, usage.OutputToken)
	}

	// chat completion mode
	if !endOfStream {
		return data
	}

	if ctx.GetContext(tokenusage.CtxKeyInputToken) == nil || ctx.GetContext(tokenusage.CtxKeyOutputToken) == nil || ctx.GetContext("apiKey") == nil {
		return data
	}

	// Only decrement quota if the API key has quota configured
	hasQuotaConfig, ok := ctx.GetContext("hasQuotaConfig").(bool)
	if !ok || !hasQuotaConfig {
		log.Debugf("apiKey has no quota configured, skipping quota decrement")
		return data
	}

	inputToken := ctx.GetContext(tokenusage.CtxKeyInputToken).(int64)
	outputToken := ctx.GetContext(tokenusage.CtxKeyOutputToken).(int64)
	apiKey := ctx.GetContext("apiKey").(string)
	routeName, _ := ctx.GetContext("routeName").(string)
	totalToken := int(inputToken + outputToken)
	log.Debugf("update apiKey:%s, routeName:%s, totalToken:%d", apiKey, routeName, totalToken)
	redisKey := getRedisKey(config, apiKey, routeName)
	config.redisClient.DecrBy(redisKey, totalToken, nil)
	return data
}

func deniedNoApiKey() types.Action {
	util.SendResponse(http.StatusUnauthorized, "ai-quota-apikey.no_key", "text/plain", "Request denied by ai quota check. No API Key found.")
	return types.ActionContinue
}

func deniedUnauthorizedApiKey() types.Action {
	util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized API Key.")
	return types.ActionContinue
}

func getOperationMode(path string, adminPath string) (ChatMode, AdminMode) {
	fullAdminPath := "/v1/chat/completions" + adminPath
	if strings.HasSuffix(path, fullAdminPath+"/refresh") {
		return ChatModeAdmin, AdminModeRefresh
	}
	if strings.HasSuffix(path, fullAdminPath+"/delta") {
		return ChatModeAdmin, AdminModeDelta
	}
	if strings.HasSuffix(path, fullAdminPath) {
		return ChatModeAdmin, AdminModeQuery
	}
	if strings.HasSuffix(path, "/v1/chat/completions") {
		return ChatModeCompletion, AdminModeNone
	}
	return ChatModeNone, AdminModeNone
}

func refreshQuota(ctx wrapper.HttpContext, config QuotaConfig, adminApiKey string, body string) types.Action {
	// check admin api key
	if adminApiKey != config.AdminApiKey {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized admin API Key.")
		return types.ActionContinue
	}

	queryValues, _ := url.ParseQuery(body)
	values := make(map[string]string, len(queryValues))
	for k, v := range queryValues {
		values[k] = v[0]
	}
	queryApiKey := values["api_key"]
	quota, err := strconv.Atoi(values["quota"])
	if queryApiKey == "" || err != nil {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. api_key can't be empty and quota must be integer.")
		return types.ActionContinue
	}
	// Get route_name from request parameter, fallback to config
	routeName := values["route_name"]
	if routeName == "" {
		routeName = config.RouteName
	}
	redisKey := getRedisKey(config, queryApiKey, routeName)
	err2 := config.redisClient.Set(redisKey, quota, func(response resp.Value) {
		log.Debugf("Redis set key = %s quota = %d", redisKey, quota)
		if err := response.Error(); err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return
		}
		util.SendResponse(http.StatusOK, "ai-quota-apikey.refreshquota", "text/plain", "refresh quota successful")
	})

	if err2 != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err2))
		return types.ActionContinue
	}

	return types.ActionPause
}

func queryQuota(ctx wrapper.HttpContext, config QuotaConfig, adminApiKey string, url *url.URL) types.Action {
	// check admin api key
	if adminApiKey != config.AdminApiKey {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized admin API Key.")
		return types.ActionContinue
	}
	// check url
	queryValues := url.Query()
	values := make(map[string]string, len(queryValues))
	for k, v := range queryValues {
		values[k] = v[0]
	}
	if values["api_key"] == "" {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. api_key can't be empty.")
		return types.ActionContinue
	}
	queryApiKey := values["api_key"]
	// Get route_name from request parameter, fallback to config
	routeName := values["route_name"]
	if routeName == "" {
		routeName = config.RouteName
	}
	redisKey := getRedisKey(config, queryApiKey, routeName)
	err := config.redisClient.Get(redisKey, func(response resp.Value) {
		quota := 0
		if err := response.Error(); err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return
		} else if response.IsNull() {
			quota = 0
		} else {
			quota = response.Integer()
		}
		result := struct {
			ApiKey    string `json:"api_key"`
			RouteName string `json:"route_name,omitempty"`
			Quota     int    `json:"quota"`
		}{
			ApiKey:    queryApiKey,
			RouteName: routeName,
			Quota:     quota,
		}
		body, _ := json.Marshal(result)
		util.SendResponse(http.StatusOK, "ai-quota-apikey.queryquota", "application/json", string(body))
	})
	if err != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
		return types.ActionContinue
	}
	return types.ActionPause
}

func deltaQuota(ctx wrapper.HttpContext, config QuotaConfig, adminApiKey string, body string) types.Action {
	// check admin api key
	if adminApiKey != config.AdminApiKey {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized admin API Key.")
		return types.ActionContinue
	}

	queryValues, _ := url.ParseQuery(body)
	values := make(map[string]string, len(queryValues))
	for k, v := range queryValues {
		values[k] = v[0]
	}
	queryApiKey := values["api_key"]
	value, err := strconv.Atoi(values["value"])
	if queryApiKey == "" || err != nil {
		util.SendResponse(http.StatusForbidden, "ai-quota-apikey.unauthorized", "text/plain", "Request denied by ai quota check. api_key can't be empty and value must be integer.")
		return types.ActionContinue
	}
	// Get route_name from request parameter, fallback to config
	routeName := values["route_name"]
	if routeName == "" {
		routeName = config.RouteName
	}
	redisKey := getRedisKey(config, queryApiKey, routeName)
	if value >= 0 {
		err := config.redisClient.IncrBy(redisKey, value, func(response resp.Value) {
			log.Debugf("Redis Incr key = %s value = %d", redisKey, value)
			if err := response.Error(); err != nil {
				util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
				return
			}
			util.SendResponse(http.StatusOK, "ai-quota-apikey.deltaquota", "text/plain", "delta quota successful")
		})
		if err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return types.ActionContinue
		}
	} else {
		err := config.redisClient.DecrBy(redisKey, 0-value, func(response resp.Value) {
			log.Debugf("Redis Decr key = %s value = %d", redisKey, 0-value)
			if err := response.Error(); err != nil {
				util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
				return
			}
			util.SendResponse(http.StatusOK, "ai-quota-apikey.deltaquota", "text/plain", "delta quota successful")
		})
		if err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota-apikey.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return types.ActionContinue
		}
	}

	return types.ActionPause
}
