package main

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

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
	ctxKeyBillingKind     = "billing_kind"
	ctxKeyAsrBuffer       = "asr_buffer"

	// JSON field names
	fieldModel              = "model"
	fieldStream             = "stream"
	fieldStreamOptionsUsage = "stream_options.include_usage"

	// Response headers from TEI
	headerComputeTokens = "x-compute-tokens"

	// Default values
	defaultBlacklistPrefix = "gen-studio"

	// Multimodal billing path suffixes (console-ui#1666)
	pathSuffixSpeech        = "/v1/audio/speech"
	pathSuffixTranscription = "/v1/audio/transcriptions"
	pathSuffixImages        = "/v1/images/generations"
	// MiniMax 原生 TTS 端点 (console-ui#2251)。与 /v1/audio/speech 同为文本转语音,
	// 只是走厂商原生协议:文本字段叫 text(OpenAI 标准叫 input)。
	// 计量口径同样取**请求体字符数** —— 与 speech 一致,不读响应体:
	// TTS 响应体是音频(实测 6 秒 mp3 的 hex 就有 196KB,流式 478KB),
	// 缓冲整个响应会撑爆 wasm 内存并把音频压到 EOS 才下发。
	pathSuffixT2aV2 = "/v1/t2a_v2"

	// billing_kind values
	billingKindSpeech        = "speech"
	billingKindTranscription = "transcription"
	billingKindImages        = "images"
	billingKindT2aV2         = "t2a_v2"

	// Whitelist / cap constants for billing dims extraction
	defaultImageCount  = 1
	maxImageCount      = 32
	maxDurationSeconds = 86400

	// Filter state key passed to proxywasm.SetProperty (see writeBillingDims).
	// Envoy's wasm Context::setProperty auto-prepends "wasm." to whatever key
	// is given here, so this constant must NOT include that prefix — the
	// access log formatter reads it back as %FILTER_STATE(wasm.billing_dims:PLAIN)%.
	filterStateKeyBillingDims = "billing_dims"
)

var (
	sizeRe    = regexp.MustCompile(`^\d{2,5}x\d{2,5}$`)
	qualityRe = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)
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

	// Multimodal billing paths (speech/images/transcription) also need to be gated
	// on the enabled-suffixes config, same as the chat-completions blacklist path.
	kind := ""
	if isPathEnabled(path, config.EnablePathSuffixes) {
		kind = billingKindOf(path)
	}
	if kind != "" {
		ctx.SetContext(ctxKeyBillingKind, kind)
	}

	// Non-rerank/non-embedding paths: only need request body processing (stream usage
	// injection), except transcription which needs the response body to read usage.seconds.
	if kind != billingKindTranscription {
		ctx.DontReadResponseBody()
	}

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

	// Multimodal billing dims: speech/t2a_v2/images are computed from the request body
	// here; transcription is computed from the response body (see
	// onHttpStreamingResponseBody) since it needs usage.seconds, so it just
	// passes through here.
	if kind, ok := ctx.GetContext(ctxKeyBillingKind).(string); ok && kind != "" {
		switch kind {
		case billingKindSpeech:
			writeBillingDims(ctx, speechDims(body), log)
		case billingKindT2aV2:
			writeBillingDims(ctx, t2aV2Dims(body), log)
		case billingKindImages:
			writeBillingDims(ctx, imageDims(body), log)
		}
		return types.ActionContinue
	}

	modelName := gjson.GetBytes(body, fieldModel).String()

	// 注:原本这里有个 AddHttpRequestHeader("x-rise-user-model", modelName) 用来给
	// access log 暴露用户原始模型名,已废弃。alibaba/higress 主仓的 model-router wasm
	// 在 AUTHN/900 phase 就把同样的值写进了标准 header x-higress-llm-model,
	// access log 直接 %REQ(x-higress-llm-model)% 取即可(见 higress-helm 仓
	// charts/higress-core/templates/configmap.yaml d271f62 commit)。
	// 我们这个插件在 UNSPECIFIED phase priority 100 跑得太晚,header 写了 access log
	// 也不一定能抓到。这里只保留 modelName 提取用于下方 rerank / blacklist 分支。

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
// Processes rerank and embedding responses — other paths (including transcription, which reads
// usage.seconds from the response body without rewriting it) already resolved their
// DontReadResponseBody() call in the request headers phase.
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
// For transcription requests, it buffers the response only to read usage.seconds for billing dims,
// passing the body through unmodified. For rerank requests, it wraps the raw array response with
// usage info from TEI headers. For embedding requests, it overwrites the response body's model
// field with the request-side model so downstream metrics (ai-statistics) reflect the gateway alias
// rather than the backend's internal path. Using streaming mode ensures the modified body is visible
// to downstream plugins (ai-statistics, ai-quota-apikey).
func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config PluginConfig, data []byte, endOfStream bool, log log.Log) []byte {
	// Transcription (ASR) billing dims: buffer the response like the rerank
	// branch below, but pass the assembled body through unmodified — we only
	// need to read usage.seconds out of it, never rewrite it.
	if kind, ok := ctx.GetContext(ctxKeyBillingKind).(string); ok && kind == billingKindTranscription {
		if !endOfStream {
			buf, _ := ctx.GetContext(ctxKeyAsrBuffer).([]byte)
			buf = append(buf, data...)
			ctx.SetContext(ctxKeyAsrBuffer, buf)
			return nil // don't send anything downstream yet
		}

		body := data
		if buf, ok := ctx.GetContext(ctxKeyAsrBuffer).([]byte); ok && len(buf) > 0 {
			body = append(buf, data...)
		}

		if dims := transcriptionDims(body); dims != "" {
			writeBillingDims(ctx, dims, log)
		}
		return body
	}

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

// billingKindOf classifies a request path into a multimodal billing kind
// ("speech" / "transcription" / "images"), or "" if the path is not one of
// the three special-cased billing paths. Matched by suffix, after stripping
// any query string (console-ui#1666).
func billingKindOf(path string) string {
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	switch {
	case strings.HasSuffix(path, pathSuffixSpeech):
		return billingKindSpeech
	case strings.HasSuffix(path, pathSuffixTranscription):
		return billingKindTranscription
	case strings.HasSuffix(path, pathSuffixImages):
		return billingKindImages
	case strings.HasSuffix(path, pathSuffixT2aV2):
		return billingKindT2aV2
	default:
		return ""
	}
}

// speechDims extracts the TTS billing dimension from the request body: the
// rune count of the "input" field. Returns "" if input is empty/missing so
// callers skip writing billing dims entirely.
func speechDims(body []byte) string {
	input := gjson.GetBytes(body, "input").String()
	if input == "" {
		return ""
	}
	return fmt.Sprintf(`{"chars":%d}`, utf8.RuneCountInString(input))
}

// t2aV2Dims — MiniMax 原生 TTS (/v1/t2a_v2) 的计量维度 (console-ui#2251)。
// 与 speechDims 同口径(请求体字符数 → {"chars":N}),只是字段名 text 而非 input。
// 复用 chars 这个既有 key,fluent-bit 的 derive_billing 无需改动即可提取。
//
// 为什么不取响应体 extra_info.usage_characters(厂商计费字符数,实测同一次调用
// 入参 25 字 / word_count 23 / usage_characters 43,三者互不相等):
// 那要求缓冲整个响应体,而 TTS 响应体是音频 hex(实测 6 秒非流式 196KB、流式 478KB),
// 内存与时延代价都不可接受。请求侧字符数与 /v1/audio/speech 现有口径一致。
func t2aV2Dims(body []byte) string {
	// 🔴 必须校验类型再取值:Result.String() 对数组/对象会返回**原始 JSON 文本**
	// (如 {"text":["ab"]} → `["ab"]` → 数成 6 个字符)、对数字/布尔会字符串化。
	// 这个数直接进计费公式,坏输入宁可不采(不写 dims)也不能凭空数出一个字符数。
	t := gjson.GetBytes(body, "text")
	if t.Type != gjson.String {
		return ""
	}
	text := t.String()
	if text == "" {
		return ""
	}
	return fmt.Sprintf(`{"chars":%d}`, utf8.RuneCountInString(text))
}

// imageDims extracts sync image-gen billing dimensions from the request
// body: quality/size/n, whitelisted against sizeRe/qualityRe. size is the
// mandatory dimension — missing or invalid size yields "" (no dims at all).
// An invalid quality is dropped on its own (size/n are still emitted). n
// defaults to 1 when absent and is capped at maxImageCount.
func imageDims(body []byte) string {
	size := gjson.GetBytes(body, "size").String()
	if size == "" || !sizeRe.MatchString(size) {
		return ""
	}

	n := defaultImageCount
	if nVal := gjson.GetBytes(body, "n"); nVal.Exists() {
		n = int(nVal.Int())
		if n < 1 {
			n = defaultImageCount
		} else if n > maxImageCount {
			n = maxImageCount
		}
	}

	result := []byte("{}")
	if quality := gjson.GetBytes(body, "quality").String(); quality != "" && qualityRe.MatchString(quality) {
		result, _ = sjson.SetBytes(result, "quality", quality)
	}
	result, _ = sjson.SetBytes(result, "size", size)
	result, _ = sjson.SetBytes(result, "n", n)
	return string(result)
}

// transcriptionDims extracts ASR billing dimensions from the RESPONSE body:
// usage.seconds, but only when usage.type == "duration" (other usage shapes,
// e.g. token counts, are not guessed at). Seconds are rounded up (billing
// favors the provider) and capped at maxDurationSeconds.
func transcriptionDims(body []byte) string {
	if gjson.GetBytes(body, "usage.type").String() != "duration" {
		return ""
	}
	seconds := gjson.GetBytes(body, "usage.seconds").Float()
	if seconds <= 0 {
		return ""
	}
	durationSeconds := int(math.Ceil(seconds))
	if durationSeconds > maxDurationSeconds {
		durationSeconds = maxDurationSeconds
	}
	return fmt.Sprintf(`{"duration_seconds":%d}`, durationSeconds)
}

// writeBillingDims writes the billing-dims JSON string (as produced by
// speechDims/imageDims/transcriptionDims) into Envoy filter state under key
// "billing_dims" — WITHOUT a "wasm." prefix. Envoy's wasm
// Context::setProperty auto-prepends "wasm." to any property path written
// via proxywasm.SetProperty (source/extensions/common/wasm/context.cc), so
// the value actually lands under "wasm.billing_dims" in filter state. This
// auto-prefixing is also why the vendored wrapper's own
// CustomLogKey = "custom_log" (pkg/wrapper/plugin_wrapper.go) is defined
// without a "wasm." prefix — same in-repo convention, don't add it here.
//
// Escaping (Task 16 e2e, console-ui#1666): the stored bytes must be the
// JSON-STRING-ESCAPED form of the flat dims, not the raw flat JSON. The
// access log format string is itself a JSON template, e.g.
// `"billing_dims":"%FILTER_STATE(wasm.billing_dims:PLAIN)%"`, and
// %FILTER_STATE(...:PLAIN)% is a byte-level splice — NOT JSON-aware. Writing
// the raw flat JSON `{"chars":156}` produces the log line
// `"billing_dims":"{"chars":156}"`, which is invalid JSON (unescaped inner
// quotes break the outer string), so fluent-bit's JSON parser silently DROPS
// the entire access log line (ai_log included) for every multimodal call —
// this is the live 4pd regression Task 16 root-caused. Escaping the dims to
// `{\"chars\":156}` before SetProperty keeps the embedded log line valid
// JSON; fluent-bit's outer JSON parse un-escapes it back to the flat
// `{"chars":156}` string, so Task 12's Lua regex (`"chars":(%d+)` etc.)
// needs zero change. This is exactly the embedding contract the wasm-go
// wrapper's WriteUserAttributeToLogWithKey/MarshalStr mechanism implements
// (see Task 10's original PoC note, which wrongly dismissed that escaping as
// unwanted "double-escaping" — it is required here, we just apply it
// ourselves via escapeJSONString rather than switching to that wrapper API,
// since the wrapper also writes under the wrong key without "wasm." control,
// see the key-prefix note above).
func writeBillingDims(ctx wrapper.HttpContext, dims string, log log.Log) {
	if dims == "" {
		return
	}
	escaped := escapeJSONString(dims)
	if err := proxywasm.SetProperty([]string{filterStateKeyBillingDims}, []byte(escaped)); err != nil {
		log.Warnf("[%s] failed to write billing dims to filter state: %v", pluginName, err)
	}
}

// escapeJSONString escapes s so it can be embedded as the value of a JSON
// string field via literal byte substitution (as Envoy's
// %FILTER_STATE(...:PLAIN)% access-log formatter does). Implemented via
// strconv.Quote with the surrounding quotes stripped: this gives us the
// standard Go/JSON escape rules (backslash and quote first, plus control
// chars like \n/\r/\t) for free instead of hand-rolling escape ordering,
// even though our whitelisted dims values (digits, ASCII letters, and the
// fixed JSON punctuation from speechDims/imageDims/transcriptionDims) never
// actually contain backslashes or control characters.
func escapeJSONString(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
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
