package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName = "model-analytics"

	// Context keys
	ctxKeySkipProcessing  = "skip_processing"
	ctxKeyIsRerank        = "is_rerank"
	ctxKeyRerankModel     = "rerank_model"
	ctxKeyComputeTokens   = "compute_tokens"
	ctxKeyRerankBuffer    = "rerank_buffer"
	ctxKeyIsEmbedding     = "is_embedding"
	ctxKeyEmbeddingModel  = "embedding_model"
	ctxKeyEmbeddingBuffer = "embedding_buffer"

	// JSON field names
	fieldModel              = "model"
	fieldStream             = "stream"
	fieldStreamOptionsUsage = "stream_options.include_usage"

	// Response headers from TEI
	headerComputeTokens = "x-compute-tokens"

	// Default values
	defaultBlacklistPrefix = "gen-studio"
)

// PluginConfig defines the plugin configuration.
type PluginConfig struct {
	// ModelWhitelist contains model names that should be skipped (exact match).
	ModelWhitelist []string `yaml:"model_whitelist" json:"model_whitelist"`
	// ModelBlacklistPrefixes contains model name prefixes that should be processed (prefix match).
	ModelBlacklistPrefixes []string `yaml:"model_blacklist_prefixes" json:"model_blacklist_prefixes"`
	// EnablePathSuffixes contains path suffixes that enable processing.
	EnablePathSuffixes []string `yaml:"enable_path_suffixes" json:"enable_path_suffixes"`
}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfigBy(parseConfig),
		wrapper.ProcessRequestHeadersBy(onHttpRequestHeaders),
		wrapper.ProcessRequestBodyBy(onHttpRequestBody),
		wrapper.ProcessResponseHeadersBy(onHttpResponseHeaders),
		wrapper.ProcessStreamingResponseBodyBy(onHttpStreamingResponseBody),
	)
}

// parseConfig parses the plugin configuration from JSON.
func parseConfig(json gjson.Result, config *PluginConfig, log log.Log) error {
	config.ModelWhitelist = parseStringArray(json, "model_whitelist")
	config.ModelBlacklistPrefixes = parseStringArray(json, "model_blacklist_prefixes")
	config.EnablePathSuffixes = parsePathSuffixes(json, "enable_path_suffixes")

	// Set default blacklist prefix if empty
	if len(config.ModelBlacklistPrefixes) == 0 {
		config.ModelBlacklistPrefixes = []string{defaultBlacklistPrefix}
	}

	return nil
}

// parseStringArray extracts a string array from JSON config.
func parseStringArray(json gjson.Result, key string) []string {
	arr := json.Get(key).Array()
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		result = append(result, item.String())
	}
	return result
}

// parsePathSuffixes extracts path suffixes from JSON config.
// If "*" is found, returns empty slice to enable all paths.
func parsePathSuffixes(json gjson.Result, key string) []string {
	arr := json.Get(key).Array()
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		s := item.String()
		if s == "*" {
			return []string{}
		}
		result = append(result, s)
	}
	return result
}

// onHttpRequestHeaders handles the request headers phase.
func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig, log log.Log) types.Action {
	path, _ := proxywasm.GetHttpRequestHeader(":path")

	// Check if this is a rerank request — needs request body (model name) and response body processing
	if isRerankPath(path) {
		log.Infof("[%s] detected rerank request: %s", pluginName, path)
		ctx.SetContext(ctxKeyIsRerank, true)
		return types.ActionContinue
	}

	// Check if this is an embedding request — needs to rewrite the response body's model field
	// so that downstream metrics reflect the gateway-facing model alias instead of the backend's
	// internal model path (e.g. TEI returns "/models/bge-m3").
	if isEmbeddingPath(path) {
		log.Infof("[%s] detected embedding request: %s", pluginName, path)
		ctx.SetContext(ctxKeyIsEmbedding, true)
		return types.ActionContinue
	}

	// Other paths: only need request body processing (stream usage injection)
	ctx.DontReadResponseBody()

	if !isPathEnabled(path, config.EnablePathSuffixes) {
		log.Debugf("[%s] skipping path %s (not in enabled suffixes)", pluginName, path)
		ctx.SetContext(ctxKeySkipProcessing, true)
		ctx.DontReadRequestBody()
	}
	return types.ActionContinue
}

// onHttpRequestBody handles the request body phase.
func onHttpRequestBody(ctx wrapper.HttpContext, config PluginConfig, body []byte, log log.Log) types.Action {
	if ctx.GetBoolContext(ctxKeySkipProcessing, false) {
		return types.ActionContinue
	}

	modelName := gjson.GetBytes(body, fieldModel).String()

	// 暴露原始(改写前)模型名给下游观测链路。
	// ai-route 在更晚阶段按 modelMapping 改写 request body 的 model 字段,ai-statistics
	// 采到的就是改写后的上游名(如 gpt-5.4),CH 里也只有这个值。这里在 body 阶段(改写前)
	// 把用户原始输入塞进 request header,Envoy access log 通过 %REQ(x-rise-user-model)%
	// 抓出来,fluent-bit 写进 CH 列 user_facing_model,模型总览各页才能按"用户调用名"聚合。
	if modelName != "" {
		if err := proxywasm.AddHttpRequestHeader("x-rise-user-model", modelName); err != nil {
			log.Warnf("[%s] failed to set x-rise-user-model header: %v", pluginName, err)
		}
	}

	// For rerank requests, save model name for response processing
	if ctx.GetBoolContext(ctxKeyIsRerank, false) {
		ctx.SetContext(ctxKeyRerankModel, modelName)
		log.Infof("[%s] rerank request model: %s", pluginName, modelName)
		return types.ActionContinue
	}

	// For embedding requests, save model name so we can overwrite the response model field
	if ctx.GetBoolContext(ctxKeyIsEmbedding, false) {
		ctx.SetContext(ctxKeyEmbeddingModel, modelName)
		log.Infof("[%s] embedding request model: %s", pluginName, modelName)
		return types.ActionContinue
	}

	// Check whitelist first (exact match) - skip if matched
	if isInWhitelist(modelName, config.ModelWhitelist) {
		log.Debugf("[%s] model %s in whitelist, skipping", pluginName, modelName)
		return types.ActionContinue
	}

	// Check blacklist (prefix match) - process if matched
	if isInBlacklist(modelName, config.ModelBlacklistPrefixes) {
		newBody := ensureStreamUsage(body, log)
		if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
			log.Errorf("[%s] failed to replace request body: %v", pluginName, err)
		}
	}

	return types.ActionContinue
}

// onHttpResponseHeaders handles the response headers phase.
// Processes rerank and embedding responses — other paths already called DontReadResponseBody in request headers.
func onHttpResponseHeaders(ctx wrapper.HttpContext, config PluginConfig, log log.Log) types.Action {
	if ctx.GetBoolContext(ctxKeyIsRerank, false) {
		// Read x-compute-tokens header and save to context (cannot read response headers in body phase)
		if tokenHeader, err := proxywasm.GetHttpResponseHeader(headerComputeTokens); err == nil && tokenHeader != "" {
			if v, err := strconv.Atoi(tokenHeader); err == nil {
				ctx.SetContext(ctxKeyComputeTokens, v)
				log.Infof("[%s] got %s: %d", pluginName, headerComputeTokens, v)
			}
		}
		// Remove content-length since we'll modify the body
		proxywasm.RemoveHttpResponseHeader("content-length")
		return types.ActionContinue
	}

	if ctx.GetBoolContext(ctxKeyIsEmbedding, false) {
		// We will overwrite the response body's model field, so the byte length will likely change.
		proxywasm.RemoveHttpResponseHeader("content-length")
		return types.ActionContinue
	}

	return types.ActionContinue
}

// onHttpStreamingResponseBody handles the streaming response body phase.
// For rerank requests, it wraps the raw array response with usage info from TEI headers.
// For embedding requests, it overwrites the response body's model field with the request-side model
// so downstream metrics (ai-statistics) reflect the gateway alias rather than the backend's internal path.
// Using streaming mode ensures the modified body is visible to downstream plugins (ai-statistics, ai-quota-apikey).
func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config PluginConfig, data []byte, endOfStream bool, log log.Log) []byte {
	if ctx.GetBoolContext(ctxKeyIsRerank, false) {
		return processRerankStreamingBody(ctx, data, endOfStream, log)
	}
	if ctx.GetBoolContext(ctxKeyIsEmbedding, false) {
		return processEmbeddingStreamingBody(ctx, data, endOfStream, log)
	}
	return data
}

// processRerankStreamingBody buffers the rerank response and, on endOfStream, transforms TEI's raw
// array body into the standard rerank response object with model and usage fields populated.
func processRerankStreamingBody(ctx wrapper.HttpContext, data []byte, endOfStream bool, log log.Log) []byte {
	// For non-streaming rerank responses, buffer chunks until endOfStream
	if !endOfStream {
		buf, _ := ctx.GetContext(ctxKeyRerankBuffer).([]byte)
		buf = append(buf, data...)
		ctx.SetContext(ctxKeyRerankBuffer, buf)
		return nil // don't send anything downstream yet
	}

	// endOfStream: assemble the full body
	body := data
	if buf, ok := ctx.GetContext(ctxKeyRerankBuffer).([]byte); ok && len(buf) > 0 {
		body = append(buf, data...)
	}

	// Get token count saved from response headers phase
	computeTokens := 0
	if v := ctx.GetContext(ctxKeyComputeTokens); v != nil {
		computeTokens = v.(int)
	}

	// Get model name saved from request phase
	modelName := ""
	if v := ctx.GetContext(ctxKeyRerankModel); v != nil {
		modelName = v.(string)
	}

	log.Infof("[%s] rerank response: model=%s, compute_tokens=%d", pluginName, modelName, computeTokens)

	// If response already contains "usage", it's from a standard-compliant engine (vLLM, Cohere, Jina, TEI v1.10+)
	// No transformation needed — pass through as-is
	if gjson.GetBytes(body, "usage").Exists() || gjson.GetBytes(body, "meta.tokens").Exists() {
		log.Infof("[%s] rerank response already contains usage, skipping transformation", pluginName)
		return body
	}

	// TEI raw array response: [{"index":0,"score":0.99}, ...]
	// Transform to standard format with usage info from x-compute-tokens header
	return buildRerankResponseWithUsage(body, modelName, computeTokens)
}

// processEmbeddingStreamingBody buffers the embedding response and, on endOfStream, replaces the
// response body's model field with the request-side model. The backend (e.g. TEI) often returns its
// own internal path like "/models/bge-m3" which would otherwise leak into downstream metrics.
func processEmbeddingStreamingBody(ctx wrapper.HttpContext, data []byte, endOfStream bool, log log.Log) []byte {
	if !endOfStream {
		buf, _ := ctx.GetContext(ctxKeyEmbeddingBuffer).([]byte)
		buf = append(buf, data...)
		ctx.SetContext(ctxKeyEmbeddingBuffer, buf)
		return nil
	}

	body := data
	if buf, ok := ctx.GetContext(ctxKeyEmbeddingBuffer).([]byte); ok && len(buf) > 0 {
		body = append(buf, data...)
	}

	modelName := ""
	if v := ctx.GetContext(ctxKeyEmbeddingModel); v != nil {
		modelName = v.(string)
	}
	if modelName == "" {
		return body
	}

	current := gjson.GetBytes(body, fieldModel).String()
	if current == modelName {
		return body
	}

	rewritten, err := sjson.SetBytes(body, fieldModel, modelName)
	if err != nil {
		log.Errorf("[%s] failed to rewrite embedding model field: %v", pluginName, err)
		return body
	}
	log.Infof("[%s] embedding response model rewritten: %q -> %q", pluginName, current, modelName)
	return rewritten
}

// buildRerankResponseWithUsage wraps the TEI rerank array response into an object with usage info.
// Output follows the Jina/OpenAI-compatible rerank response format.
func buildRerankResponseWithUsage(body []byte, model string, promptTokens int) []byte {
	// Normalize TEI results: rename "score" → "relevance_score"
	normalizedResults := normalizeRerankResults(body)

	result := []byte("{}")
	if model != "" {
		result, _ = sjson.SetBytes(result, "model", model)
	}
	result, _ = sjson.SetBytes(result, "object", "list")
	result, _ = sjson.SetRawBytes(result, "results", normalizedResults)
	result, _ = sjson.SetBytes(result, "usage.prompt_tokens", promptTokens)
	result, _ = sjson.SetBytes(result, "usage.total_tokens", promptTokens)

	return result
}

// normalizeRerankResults renames "score" to "relevance_score" in each result item.
// TEI returns: [{"index":0,"score":0.99}]
// Standard:   [{"index":0,"relevance_score":0.99}]
func normalizeRerankResults(body []byte) []byte {
	items := gjson.ParseBytes(body).Array()
	if len(items) == 0 {
		return body
	}

	result := body
	for i := len(items) - 1; i >= 0; i-- {
		score := items[i].Get("score")
		if !score.Exists() {
			continue
		}
		path := fmt.Sprintf("%d.relevance_score", i)
		result, _ = sjson.SetBytes(result, path, score.Float())
		deletePath := fmt.Sprintf("%d.score", i)
		result, _ = sjson.DeleteBytes(result, deletePath)
	}
	return result
}

// isInWhitelist checks if model name exactly matches any whitelist entry.
func isInWhitelist(modelName string, whitelist []string) bool {
	for _, name := range whitelist {
		if modelName == name {
			return true
		}
	}
	return false
}

// isInBlacklist checks if model name has any blacklist prefix.
func isInBlacklist(modelName string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(modelName, prefix) {
			return true
		}
	}
	return false
}

// isRerankPath checks if the request path is a rerank endpoint.
// Matches both "/rerank" and "/v1/rerank" (and any suffix-equivalent).
func isRerankPath(path string) bool {
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	return strings.HasSuffix(path, "/rerank")
}

// isEmbeddingPath checks if the request path is an embedding endpoint.
// Matches "/embeddings" and "/v1/embeddings".
func isEmbeddingPath(path string) bool {
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	return strings.HasSuffix(path, "/embeddings")
}

// isPathEnabled checks if the request path matches any enabled suffix.
// Returns true if no suffixes are configured (all paths enabled).
func isPathEnabled(path string, suffixes []string) bool {
	if len(suffixes) == 0 {
		return true
	}

	// Strip query parameters
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}

	for _, suffix := range suffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// ensureStreamUsage ensures stream_options.include_usage is true for streaming requests.
func ensureStreamUsage(body []byte, log log.Log) []byte {
	if !gjson.GetBytes(body, fieldStream).Bool() {
		return body
	}

	usage := gjson.GetBytes(body, fieldStreamOptionsUsage)
	if usage.Exists() && usage.Bool() {
		return body
	}

	newBody, err := sjson.SetBytes(body, fieldStreamOptionsUsage, true)
	if err != nil {
		log.Errorf("[%s] failed to set include_usage: %v", pluginName, err)
		return body
	}
	return newBody
}
