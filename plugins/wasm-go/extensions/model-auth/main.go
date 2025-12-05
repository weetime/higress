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
// @Description zh-CN 本插件实现了基于 API Key、租户和工作空间的模型访问控制功能。
// @Description en-US This plugin implements model access control based on API Key, tenants and workspaces.
// @IconUrl https://img.alicdn.com/imgextra/i4/O1CN01BPFGlT1pGZ2VDLgaH_!!6000000005333-2-tps-42-42.png
// @Version 0.0.13
//
// @Contact.name Higress Team
// @Contact.url http://higress.io/
// @Contact.email admin@higress.io
//
// @Example
// model_mapping:
//
//	model-a:
//	  - "*"
//	model-b:
//	  - "tenant-1"
//	  - "tenant-2"
//
// workspace_mapping:
//
//	tenant-1:
//	  - "user-1"
//	  - "user-2"
//	tenant-2:
//	  - "user-3"
//
// api_key_mapping:
//
//	user-1:
//	  - "sk-02a69aa1-df0e-4ecb-8e02-a0b832d56295"
//	  - "sk-1d25a264-fad1-46de-95e7-54621d827d7e"
//	user-2:
//	  - "sk-e377ddd2-88f2-47cb-9e2c-fe174dca1bd0"
//
// auth_header_name: Authorization
// model_header_name: x-higress-llm-model
// @End
type ModelAuthConfig struct {
	// @Title 模型到租户的映射关系
	// @Title en-US Model to Tenants Mapping
	// @Description 模型名称到允许访问的租户列表的映射。如果租户列表包含 "*"，则所有用户都可以访问。
	// @Description en-US Mapping from model name to list of allowed tenants. If tenant list contains "*", all users can access.
	modelMapping map[string][]string

	// @Title 租户到用户的映射关系
	// @Title en-US Tenant to Users Mapping
	// @Description 租户名称到该租户下用户列表的映射。
	// @Description en-US Mapping from tenant name to list of users in that tenant.
	workspaceMapping map[string][]string

	// @Title API Key 到用户的映射关系
	// @Title en-US API Key to User Mapping
	// @Description API Key 到用户名的映射关系。
	// @Description en-US Mapping from API Key to user name.
	apiKeyMapping map[string]string

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
//	  "model_mapping": {
//	    "model-a": ["*"],
//	    "model-b": ["tenant-1", "tenant-2"]
//	  },
//	  "workspace_mapping": {
//	    "tenant-1": ["user-1", "user-2"],
//	    "tenant-2": ["user-3"]
//	  },
//	  "api_key_mapping": {
//	    "user-1": ["sk-key-1", "sk-key-2"],
//	    "user-2": ["sk-key-3"]
//	  },
//	  "auth_header_name": "Authorization",
//	  "model_header_name": "x-higress-llm-model"
//	}
func parseConfig(json gjson.Result, config *ModelAuthConfig, log log.Log) error {
	config.modelMapping = make(map[string][]string)
	config.workspaceMapping = make(map[string][]string)
	config.apiKeyMapping = make(map[string]string)

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

	// Parse model_mapping (required)
	modelMappingJson := json.Get("model_mapping")
	if !modelMappingJson.Exists() {
		return errors.New("model_mapping is required")
	}

	modelMappingJson.ForEach(func(key, value gjson.Result) bool {
		modelName := key.String()
		tenants := make([]string, 0)
		if value.IsArray() {
			for _, tenant := range value.Array() {
				tenants = append(tenants, tenant.String())
			}
		}
		config.modelMapping[modelName] = tenants
		log.Debugf("loaded model %q with %d tenants: %v", modelName, len(tenants), tenants)
		return true
	})

	if len(config.modelMapping) == 0 {
		return errors.New("model_mapping cannot be empty")
	}

	// Parse workspace_mapping (required)
	workspaceMappingJson := json.Get("workspace_mapping")
	if !workspaceMappingJson.Exists() {
		return errors.New("workspace_mapping is required")
	}

	workspaceMappingJson.ForEach(func(key, value gjson.Result) bool {
		tenant := key.String()
		users := make([]string, 0)
		if value.IsArray() {
			for _, user := range value.Array() {
				users = append(users, user.String())
			}
		}
		config.workspaceMapping[tenant] = users
		log.Debugf("loaded tenant %q with %d users: %v", tenant, len(users), users)
		return true
	})

	if len(config.workspaceMapping) == 0 {
		return errors.New("workspace_mapping cannot be empty")
	}

	// Parse api_key_mapping (required)
	// Configuration format: username -> []apiKey
	// Internal storage: apiKey -> username (for fast lookup)
	apiKeyMappingJson := json.Get("api_key_mapping")
	if !apiKeyMappingJson.Exists() {
		return errors.New("api_key_mapping is required")
	}

	apiKeyMappingJson.ForEach(func(key, value gjson.Result) bool {
		userName := key.String()
		if value.IsArray() {
			for _, apiKey := range value.Array() {
				apiKeyStr := apiKey.String()
				config.apiKeyMapping[apiKeyStr] = userName
			}
		}
		return true
	})

	if len(config.apiKeyMapping) == 0 {
		return errors.New("api_key_mapping cannot be empty")
	}

	return nil
}

// ============================================================================
// Request Processing
// ============================================================================

// onHttpRequestHeaders handles the HTTP request headers for model authentication.
func onHttpRequestHeaders(ctx wrapper.HttpContext, config ModelAuthConfig, log log.Log) types.Action {
	// Step 1: Get model name from header, skip if not present
	modelName, err := proxywasm.GetHttpRequestHeader(config.modelHeaderName)
	if err != nil || modelName == "" {
		log.Debugf("model header %q not found, skipping", config.modelHeaderName)
		return types.ActionContinue
	}

	// Step 2: Check if model is managed by this plugin
	allowedTenants, exists := config.modelMapping[modelName]
	if !exists {
		log.Debugf("model %q not found in model_mapping, not managed by this plugin, skipping", modelName)
		return types.ActionContinue
	}

	// Step 3: Get API key from header
	authHeader, err := proxywasm.GetHttpRequestHeader(config.authHeaderName)
	if err != nil || authHeader == "" {
		log.Warnf("auth header %q is missing", config.authHeaderName)
		return deniedMissingAuthHeader(config.authHeaderName)
	}

	apiKey, err := extractAPIKey(authHeader, config.authHeaderName)
	if err != nil {
		log.Warnf("invalid auth format: %v", err)
		return deniedInvalidAuthFormat(config.authHeaderName)
	}

	// Step 4: Get user from API key and set consumer header
	userName, userExists := config.apiKeyMapping[apiKey]

	if !userExists {
		log.Warnf("API key not found in api_key_mapping")
		return deniedInvalidAPIKey()
	}

	// Set consumer header with username + last 8 chars of API key (or full key if shorter)
	apiKeySuffix := apiKey
	if len(apiKey) > 8 {
		apiKeySuffix = apiKey[len(apiKey)-8:]
	}
	_ = proxywasm.ReplaceHttpRequestHeader("x-api-key-name", userName+"/"+apiKeySuffix)
	log.Debugf("set consumer header: user=%s", userName)

	// Step 5: Check if model allows all users (wildcard "*")
	if len(allowedTenants) == 1 && allowedTenants[0] == "*" {
		log.Infof("auth success: model=%s, user=%s (wildcard)", modelName, userName)
		return types.ActionContinue
	}

	// Step 7: Check if user belongs to any of the allowed tenants
	for _, tenant := range allowedTenants {
		users, exists := config.workspaceMapping[tenant]
		if !exists {
			log.Debugf("tenant %q not found in workspace_mapping", tenant)
			continue
		}

		if contains(users, userName) {
			// User is authorized
			log.Infof("auth success: model=%s, user=%s, tenant=%s", modelName, userName, tenant)
			return types.ActionContinue
		}
	}

	// User not authorized for any of the allowed tenants
	log.Warnf("user %q not authorized for model %q (allowed tenants: %v)", userName, modelName, allowedTenants)
	return deniedAccessDenied(modelName, userName)
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

// deniedAccessDenied returns 403 when user is not authorized to access the model.
func deniedAccessDenied(modelName, userName string) types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusForbidden,
		pluginName+".access_denied",
		responseHeaders(),
		[]byte(fmt.Sprintf(`{"error":"User %s is not authorized to access model: %s"}`, userName, modelName)),
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
