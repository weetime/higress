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
	"errors"
	"fmt"
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
	pluginName = "model-auth"

	// Default header names
	defaultAuthHeaderName  = "Authorization"
	defaultModelHeaderName = "x-higress-llm-model"

	// Bearer token prefix
	bearerPrefix = "Bearer "

	// Protection space for WWW-Authenticate header
	protectionSpace = "Higress Gateway"
)

// ============================================================================
// Plugin Entry Points
// ============================================================================

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfigBy(parseConfig),
		wrapper.ProcessRequestHeadersBy(onHttpRequestHeaders),
	)
}

// ============================================================================
// Configuration Types
// ============================================================================

// APIKeyConfig represents the configuration for a single API key.
type APIKeyConfig struct {
	// @Title Consumer 名称
	// @Title en-US Consumer Name
	// @Description 该 API Key 对应的 consumer 名称，会被设置到 x-api-key-name header 中。
	// @Description en-US The consumer name for this API key, will be set to x-api-key-name header.
	Name string

	// @Title 允许访问的模型列表
	// @Title en-US Allowed Models
	// @Description 该 API Key 允许访问的模型列表。
	// @Description en-US The list of models that this API key is allowed to access.
	Models []string
}

// @Name model-auth
// @Category auth
// @Phase AUTHN
// @Priority 300
// @Title zh-CN 模型访问认证
// @Title en-US Model Access Authentication
// @Description zh-CN 本插件实现了基于 API Key 的模型访问控制功能，可以限制不同 API Key 访问特定的 LLM 模型。
// @Description en-US This plugin implements model access control based on API Key, restricting different API keys to access specific LLM models.
// @IconUrl https://img.alicdn.com/imgextra/i4/O1CN01BPFGlT1pGZ2VDLgaH_!!6000000005333-2-tps-42-42.png
// @Version 0.0.7
//
// @Contact.name Higress Team
// @Contact.url http://higress.io/
// @Contact.email admin@higress.io
//
// @Example
// api_key_models:
//
//	sk-test:
//	  name: weetime/ai-for-deployer
//	  models:
//	    - model-a
//	    - model-b
//	sk-prod:
//	  name: prod-consumer
//	  models:
//	    - model-c
//
// whitelist:
//   - model-public
//   - model-free
//
// auth_header_name: Authorization
// model_header_name: x-higress-llm-model
// @End
type ModelAuthConfig struct {
	// @Title API Key 与模型的映射关系
	// @Title en-US API Key to Models Mapping
	// @Description key 为 API Key，value 为该 Key 的配置（包含 name 和 models）。
	// @Description en-US Key is the API key, value is the configuration (including name and models).
	apiKeyModels map[string]*APIKeyConfig

	// @Title 模型白名单
	// @Title en-US Model Whitelist
	// @Description 白名单中的模型无需认证，直接放行。
	// @Description en-US Models in the whitelist are allowed without authentication.
	whitelist []string

	// @Title 认证头名称
	// @Title en-US Auth Header Name
	// @Description 包含 API Key 的请求头名称。默认为 "Authorization"。
	// @Description en-US The name of the request header containing the API key. Default is "Authorization".
	authHeaderName string

	// @Title 模型头名称
	// @Title en-US Model Header Name
	// @Description 包含模型名称的请求头名称。默认为 "x-higress-llm-model"。
	// @Description en-US The name of the request header containing the model name. Default is "x-higress-llm-model".
	modelHeaderName string
}

// ============================================================================
// Configuration Parsing
// ============================================================================

// parseConfig parses the plugin configuration from JSON.
//
// Configuration format:
//
//	{
//	  "api_key_models": {
//	    "sk-test": {
//	      "name": "weetime/ai-for-deployer",
//	      "models": ["model-a", "model-b"]
//	    },
//	    "sk-prod": {
//	      "name": "prod-consumer",
//	      "models": ["model-c", "model-d"]
//	    }
//	  },
//	  "auth_header_name": "Authorization",
//	  "model_header_name": "x-higress-llm-model"
//	}
func parseConfig(json gjson.Result, config *ModelAuthConfig, log log.Log) error {
	config.apiKeyModels = make(map[string]*APIKeyConfig)
	config.whitelist = make([]string, 0)

	// Parse auth_header_name (optional)
	config.authHeaderName = json.Get("auth_header_name").String()
	if config.authHeaderName == "" {
		config.authHeaderName = defaultAuthHeaderName
	}

	// Parse model_header_name (optional)
	config.modelHeaderName = json.Get("model_header_name").String()
	if config.modelHeaderName == "" {
		config.modelHeaderName = defaultModelHeaderName
	}

	// Parse whitelist (optional)
	whitelistJson := json.Get("whitelist")
	if whitelistJson.Exists() && whitelistJson.IsArray() {
		for _, model := range whitelistJson.Array() {
			config.whitelist = append(config.whitelist, model.String())
		}
		log.Infof("loaded whitelist with %d models: %v", len(config.whitelist), config.whitelist)
	}

	log.Infof("config: auth_header=%s, model_header=%s", config.authHeaderName, config.modelHeaderName)

	// Parse api_key_models (required)
	apiKeyModelsJson := json.Get("api_key_models")
	if !apiKeyModelsJson.Exists() {
		return errors.New("api_key_models is required")
	}

	apiKeyModelsJson.ForEach(func(key, value gjson.Result) bool {
		apiKey := key.String()
		keyConfig := &APIKeyConfig{}

		// Parse name (required)
		keyConfig.Name = value.Get("name").String()
		if keyConfig.Name == "" {
			// Fallback to apiKey as name if not specified
			keyConfig.Name = apiKey
		}

		// Parse models
		modelsJson := value.Get("models")
		if modelsJson.Exists() && modelsJson.IsArray() {
			for _, model := range modelsJson.Array() {
				keyConfig.Models = append(keyConfig.Models, model.String())
			}
		}

		config.apiKeyModels[apiKey] = keyConfig
		log.Debugf("loaded API key %q with name=%s, %d models", apiKey, keyConfig.Name, len(keyConfig.Models))
		return true
	})

	if len(config.apiKeyModels) == 0 {
		return errors.New("api_key_models cannot be empty")
	}

	return nil
}

// ============================================================================
// Request Processing
// ============================================================================

// onHttpRequestHeaders handles the HTTP request headers for model authentication.
func onHttpRequestHeaders(ctx wrapper.HttpContext, config ModelAuthConfig, log log.Log) types.Action {
	// Get model name from header, skip if not present
	modelName, err := proxywasm.GetHttpRequestHeader(config.modelHeaderName)
	if err != nil || modelName == "" {
		log.Debugf("model header %q not found, skipping", config.modelHeaderName)
		return types.ActionContinue
	}

	// Try to get keyConfig from auth header (needed for consumer header)
	keyConfig, authErr := config.getKeyConfig(config.authHeaderName)

	// Whitelist check: allow access, but still set consumer if available
	if contains(config.whitelist, modelName) {
		setConsumerHeader(keyConfig, log, modelName, true)
		return types.ActionContinue
	}

	// Non-whitelist: require valid API key
	if authErr != nil {
		log.Warnf("auth failed: %v", authErr)
		return authErr.toResponse(config.authHeaderName)
	}

	// Verify model access permission
	if !contains(keyConfig.Models, modelName) {
		log.Warnf("model %q not allowed for this API key", modelName)
		return deniedModelAccessDenied(modelName)
	}

	// Success
	setConsumerHeader(keyConfig, log, modelName, false)
	return types.ActionContinue
}

// authError represents an authentication error with its type.
type authError struct {
	errType string
	message string
}

func (e *authError) Error() string { return e.message }

func (e *authError) toResponse(headerName string) types.Action {
	switch e.errType {
	case "missing_header":
		return deniedMissingAuthHeader(headerName)
	case "invalid_format":
		return deniedInvalidAuthFormat(headerName)
	default:
		return deniedInvalidAPIKey()
	}
}

// getKeyConfig extracts API key from header and returns the corresponding config.
func (c *ModelAuthConfig) getKeyConfig(authHeaderName string) (*APIKeyConfig, *authError) {
	authHeader, err := proxywasm.GetHttpRequestHeader(authHeaderName)
	if err != nil || authHeader == "" {
		return nil, &authError{"missing_header", "auth header is missing"}
	}

	apiKey, err := extractAPIKey(authHeader, authHeaderName)
	if err != nil {
		return nil, &authError{"invalid_format", err.Error()}
	}

	keyConfig, exists := c.apiKeyModels[apiKey]
	if !exists {
		return nil, &authError{"invalid_key", "API key not found"}
	}

	return keyConfig, nil
}

// setConsumerHeader sets x-api-key-name header if keyConfig has a name.
func setConsumerHeader(keyConfig *APIKeyConfig, log log.Log, modelName string, isWhitelisted bool) {
	if keyConfig != nil && keyConfig.Name != "" {
		_ = proxywasm.ReplaceHttpRequestHeader("x-api-key-name", keyConfig.Name)
		if isWhitelisted {
			log.Infof("whitelisted model=%s, consumer=%s", modelName, keyConfig.Name)
		} else {
			log.Infof("auth success: model=%s, consumer=%s", modelName, keyConfig.Name)
		}
	} else if isWhitelisted {
		log.Infof("whitelisted model=%s", modelName)
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// extractAPIKey extracts the API key from the authentication header.
// For "Authorization" header, expects "Bearer <token>" format.
// For other headers, uses the value directly.
func extractAPIKey(headerValue, headerName string) (string, error) {
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

// contains checks if a slice contains the specified item.
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// ============================================================================
// Response Helpers
// ============================================================================

// deniedMissingAuthHeader returns 401 when auth header is missing.
func deniedMissingAuthHeader(headerName string) types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusUnauthorized,
		pluginName+".missing_auth_header",
		wwwAuthenticateHeader(protectionSpace),
		[]byte(fmt.Sprintf(`{"error":"%s header is required"}`, headerName)),
		-1,
	)
	return types.ActionContinue
}

// deniedInvalidAuthFormat returns 401 when auth header format is invalid.
func deniedInvalidAuthFormat(headerName string) types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusUnauthorized,
		pluginName+".invalid_auth_format",
		wwwAuthenticateHeader(protectionSpace),
		[]byte(fmt.Sprintf(`{"error":"Invalid %s header format"}`, headerName)),
		-1,
	)
	return types.ActionContinue
}

// deniedInvalidAPIKey returns 401 when API key is not found in configuration.
func deniedInvalidAPIKey() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusUnauthorized,
		pluginName+".invalid_api_key",
		wwwAuthenticateHeader(protectionSpace),
		[]byte(`{"error":"Invalid API key"}`),
		-1,
	)
	return types.ActionContinue
}

// deniedModelAccessDenied returns 403 when model access is not allowed.
func deniedModelAccessDenied(modelName string) types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusForbidden,
		pluginName+".model_access_denied",
		responseHeaders(),
		[]byte(fmt.Sprintf(`{"error":"Access denied for model: %s"}`, modelName)),
		-1,
	)
	return types.ActionContinue
}

// wwwAuthenticateHeader returns the WWW-Authenticate header for 401 responses.
func wwwAuthenticateHeader(realm string) [][2]string {
	return [][2]string{
		{"Content-Type", "application/json"},
		{"WWW-Authenticate", fmt.Sprintf("Bearer realm=%s", realm)},
	}
}

// responseHeaders returns standard JSON response headers.
func responseHeaders() [][2]string {
	return [][2]string{
		{"Content-Type", "application/json"},
	}
}
