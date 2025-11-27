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

// @Name model-auth
// @Category auth
// @Phase AUTHN
// @Priority 300
// @Title zh-CN 模型访问认证
// @Title en-US Model Access Authentication
// @Description zh-CN 本插件实现了基于 API Key 的模型访问控制功能，可以限制不同 API Key 访问特定的 LLM 模型。
// @Description en-US This plugin implements model access control based on API Key, restricting different API keys to access specific LLM models.
// @IconUrl https://img.alicdn.com/imgextra/i4/O1CN01BPFGlT1pGZ2VDLgaH_!!6000000005333-2-tps-42-42.png
// @Version 1.0.0
//
// @Contact.name Higress Team
// @Contact.url http://higress.io/
// @Contact.email admin@higress.io
//
// @Example
// api_key_models:
//
//	sk-test:
//	  - model-a
//	  - model-b
//	sk-prod:
//	  - model-c
//
// auth_header_name: Authorization
// model_header_name: x-higress-llm-model
// @End
type ModelAuthConfig struct {
	// @Title API Key 与模型的映射关系
	// @Title en-US API Key to Models Mapping
	// @Description key 为 API Key，value 为该 Key 允许访问的模型列表。
	// @Description en-US Key is the API key, value is the list of models that the key is allowed to access.
	apiKeyModels map[string][]string

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
//	    "sk-test": ["model-a", "model-b"],
//	    "sk-prod": ["model-c", "model-d"]
//	  },
//	  "auth_header_name": "Authorization",
//	  "model_header_name": "x-higress-llm-model"
//	}
func parseConfig(json gjson.Result, config *ModelAuthConfig, log log.Log) error {
	config.apiKeyModels = make(map[string][]string)

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

	log.Infof("config: auth_header=%s, model_header=%s", config.authHeaderName, config.modelHeaderName)

	// Parse api_key_models (required)
	apiKeyModelsJson := json.Get("api_key_models")
	if !apiKeyModelsJson.Exists() {
		return errors.New("api_key_models is required")
	}

	apiKeyModelsJson.ForEach(func(key, value gjson.Result) bool {
		apiKey := key.String()
		var models []string

		if value.IsArray() {
			for _, model := range value.Array() {
				models = append(models, model.String())
			}
		}

		config.apiKeyModels[apiKey] = models
		log.Debugf("loaded API key %q with %d models", apiKey, len(models))
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
// Authentication flow:
//  1. Check if model header exists - skip auth if not present
//  2. Extract API key from auth header
//  3. Validate API key exists in configuration
//  4. Check if the requested model is allowed for this API key
func onHttpRequestHeaders(ctx wrapper.HttpContext, config ModelAuthConfig, log log.Log) types.Action {
	// Step 1: Check model header - skip authentication if not present
	modelName, err := proxywasm.GetHttpRequestHeader(config.modelHeaderName)
	if err != nil || modelName == "" {
		log.Debugf("model header %q not found, skipping authentication", config.modelHeaderName)
		return types.ActionContinue
	}

	// Step 2: Extract API key from auth header
	authHeader, err := proxywasm.GetHttpRequestHeader(config.authHeaderName)
	if err != nil || authHeader == "" {
		log.Warnf("auth header %q is missing", config.authHeaderName)
		return deniedMissingAuthHeader(config.authHeaderName)
	}

	apiKey, err := extractAPIKey(authHeader, config.authHeaderName)
	if err != nil {
		log.Warnf("failed to extract API key: %v", err)
		return deniedInvalidAuthFormat(config.authHeaderName)
	}

	// Step 3: Validate API key exists
	allowedModels, exists := config.apiKeyModels[apiKey]
	if !exists {
		log.Warnf("API key not found in configuration")
		return deniedInvalidAPIKey()
	}

	// Step 4: Validate model access
	if !contains(allowedModels, modelName) {
		log.Warnf("model %q not allowed for this API key", modelName)
		return deniedModelAccessDenied(modelName)
	}

	// Authentication successful
	log.Infof("auth success: model=%s", modelName)
	return types.ActionContinue
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
