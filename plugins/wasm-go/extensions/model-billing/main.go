package main

import (
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName = "model-billing"

	// Context keys
	ctxKeySkipProcessing = "skip_processing"

	// JSON field names
	fieldModel              = "model"
	fieldStream             = "stream"
	fieldStreamOptionsUsage = "stream_options.include_usage"

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
	// If x-api-key-name header exists, replace x-mse-consumer header with its value
	if apiKeyName, err := proxywasm.GetHttpRequestHeader("x-api-key-name"); err == nil && apiKeyName != "" {
		proxywasm.ReplaceHttpRequestHeader("x-mse-consumer", apiKeyName)
	}

	path, _ := proxywasm.GetHttpRequestHeader(":path")
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
