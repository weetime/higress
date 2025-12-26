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
	pluginName = "ai-route-auth"

	// Default header names
	defaultAuthHeaderName = "Authorization"

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
		wrapper.ParseOverrideConfigBy(parseConfig, parseRuleConfig),
		wrapper.ProcessRequestHeadersBy(onHttpRequestHeaders),
	)
}

// ============================================================================
// Configuration Types
// ============================================================================

// RouteAuthConfig 路由认证配置
type RouteAuthConfig struct {
	// 租户到用户的映射 (workspace -> []users)
	workspaceUsers map[string][]string

	// API Key 到用户的映射 (apiKey -> username)
	// 从 user_apikeys 转换而来，用于快速查找
	apiKeyMapping map[string]string

	// 认证头名称，默认 "Authorization"
	authHeaderName string

	// 允许访问的租户列表（从 matchRules 中获取）
	allowWorkspaces []string
}

// ============================================================================
// Configuration Parsing
// ============================================================================

// parseConfig 解析 defaultConfig
//
// Configuration format:
//
//	{
//	  "workspace_users": {
//	    "ai-for-deployer": ["admin", "xiaoma"]
//	  },
//	  "user_apikeys": {
//	    "admin": ["sk-aaa", "ak-bbb"],
//	    "username2": ["sk-ccc"]
//	  },
//	  "auth_header_name": "Authorization"
//	}
func parseConfig(json gjson.Result, config *RouteAuthConfig, log log.Log) error {
	config.workspaceUsers = make(map[string][]string)
	config.apiKeyMapping = make(map[string]string)

	// Parse auth_header_name (optional)
	config.authHeaderName = json.Get("auth_header_name").String()
	if config.authHeaderName == "" {
		config.authHeaderName = defaultAuthHeaderName
	}

	log.Infof("config: auth_header=%s", config.authHeaderName)

	// Parse workspace_users (optional)
	// 如果不配置，则不进行鉴权
	workspaceUsersJson := json.Get("workspace_users")
	if workspaceUsersJson.Exists() {
		workspaceUsersJson.ForEach(func(key, value gjson.Result) bool {
			workspace := key.String()
			users := make([]string, 0)
			if value.IsArray() {
				for _, user := range value.Array() {
					users = append(users, user.String())
				}
			}
			config.workspaceUsers[workspace] = users
			log.Debugf("loaded workspace %q with %d users: %v", workspace, len(users), users)
			return true
		})
	} else {
		log.Infof("workspace_users not configured, authentication will be disabled")
	}

	// Parse user_apikeys (optional)
	// 如果不配置，则不进行鉴权
	// Configuration format: username -> []apiKey
	// Internal storage: apiKey -> username (for fast lookup)
	userApiKeysJson := json.Get("user_apikeys")
	if userApiKeysJson.Exists() {
		userApiKeysJson.ForEach(func(key, value gjson.Result) bool {
			userName := key.String()
			if value.IsArray() {
				for _, apiKey := range value.Array() {
					apiKeyStr := apiKey.String()
					config.apiKeyMapping[apiKeyStr] = userName
				}
			}
			return true
		})
	} else {
		log.Infof("user_apikeys not configured, authentication will be disabled")
	}

	if len(config.apiKeyMapping) > 0 || len(config.workspaceUsers) > 0 {
		log.Infof("loaded %d API keys for %d workspaces", len(config.apiKeyMapping), len(config.workspaceUsers))
	} else {
		log.Infof("no authentication configured, all requests will be allowed")
	}

	return nil
}

// parseRuleConfig 解析 matchRules 配置，继承 defaultConfig 的配置
//
// Configuration format:
//
//	{
//	  "rule_name": "infer-357cefd0-c54d-4d8b-9637-2beb7006c0e4",
//	  "allow_workspaces": ["*"]  // 或 ["tenant1", "tenant2"]
//	}
func parseRuleConfig(json gjson.Result, global RouteAuthConfig, config *RouteAuthConfig, log log.Log) error {
	// 继承全局配置
	*config = global

	// Parse allow_workspaces (required in matchRules)
	allowWorkspacesJson := json.Get("allow_workspaces")
	if !allowWorkspacesJson.Exists() {
		return errors.New("allow_workspaces is required in matchRules config")
	}

	config.allowWorkspaces = make([]string, 0)
	if allowWorkspacesJson.IsArray() {
		for _, workspace := range allowWorkspacesJson.Array() {
			workspaceStr := workspace.String()
			config.allowWorkspaces = append(config.allowWorkspaces, workspaceStr)
		}
	}

	if len(config.allowWorkspaces) == 0 {
		return errors.New("allow_workspaces cannot be empty")
	}

	log.Debugf("loaded allow_workspaces: %v", config.allowWorkspaces)

	// rule_name 字段仅作为配置标识，插件逻辑中不需要使用

	return nil
}

// ============================================================================
// Request Processing
// ============================================================================

// onHttpRequestHeaders 处理 HTTP 请求头，进行路由权限验证
func onHttpRequestHeaders(ctx wrapper.HttpContext, config RouteAuthConfig, log log.Log) types.Action {
	// Step 1: 检查是否有 allow_workspaces 配置
	// 如果没有匹配的 matchRules，config 会是 defaultConfig，其中没有 allow_workspaces
	// 这种情况下跳过处理（因为只有 matchRules 中才有 allow_workspaces）
	if len(config.allowWorkspaces) == 0 {
		log.Debugf("no allow_workspaces configured, skipping")
		return types.ActionContinue
	}

	// Step 1.5: 如果 workspace_users 或 user_apikeys 未配置，则不进行鉴权
	if len(config.workspaceUsers) == 0 || len(config.apiKeyMapping) == 0 {
		log.Debugf("no authentication configured (workspace_users and user_apikeys are both empty), skipping authentication")
		return types.ActionContinue
	}

	// Step 2: 提取 API Key
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

	// Step 3: 查找用户
	userName, userExists := config.apiKeyMapping[apiKey]
	if !userExists {
		log.Warnf("API key not found in user_apikeys")
		return deniedInvalidAPIKey()
	}

	// Step 4: 设置 Header
	apiKeySuffix := apiKey
	if len(apiKey) > 8 {
		apiKeySuffix = apiKey[len(apiKey)-8:]
	}
	_ = proxywasm.ReplaceHttpRequestHeader("x-api-key-name", userName+"/"+apiKeySuffix)
	log.Debugf("set consumer header: user=%s", userName)

	// Step 5: 权限验证
	// 情况A：通配符模式 (allow_workspaces 包含 "*")
	for _, workspace := range config.allowWorkspaces {
		if workspace == "*" {
			log.Infof("auth success: user=%s (wildcard)", userName)
			return types.ActionContinue
		}
	}

	// 情况B：租户列表模式 (allow_workspaces 是具体的租户数组)
	for _, workspace := range config.allowWorkspaces {
		users, exists := config.workspaceUsers[workspace]
		if !exists {
			log.Debugf("workspace %q not found in workspace_users", workspace)
			continue
		}

		if contains(users, userName) {
			// 用户在该租户的用户列表中，授权成功
			log.Infof("auth success: user=%s, workspace=%s", userName, workspace)
			return types.ActionContinue
		}
	}

	// 用户不在任何允许的租户中
	log.Warnf("user %q not authorized (allowed workspaces: %v)", userName, config.allowWorkspaces)
	return deniedAccessDenied(userName, config.allowWorkspaces)
}

// ============================================================================
// Helper Functions
// ============================================================================

// extractAPIKey 从认证头中提取 API Key
// 对于 "Authorization" header，期望 "Bearer <token>" 格式
// 对于其他 header，直接使用值
func extractAPIKey(headerValue, headerName string) (string, error) {
	if headerName == defaultAuthHeaderName {
		// Authorization header: 期望 "Bearer <token>" 格式
		if !strings.HasPrefix(headerValue, bearerPrefix) {
			return "", errors.New("bearer token not found")
		}
		apiKey := strings.TrimSpace(headerValue[len(bearerPrefix):])
		if apiKey == "" {
			return "", errors.New("empty bearer token")
		}
		return apiKey, nil
	}

	// 其他 header: 直接使用值
	apiKey := strings.TrimSpace(headerValue)
	if apiKey == "" {
		return "", errors.New("empty header value")
	}
	return apiKey, nil
}

// contains 检查切片中是否包含指定项
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

// deniedMissingAuthHeader 当认证头缺失时返回 401
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

// deniedInvalidAuthFormat 当认证头格式无效时返回 401
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

// deniedInvalidAPIKey 当 API key 未找到时返回 401
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

// deniedAccessDenied 当用户无权访问路由时返回 403
func deniedAccessDenied(userName string, allowWorkspaces []string) types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusForbidden,
		pluginName+".access_denied",
		responseHeaders(),
		[]byte(fmt.Sprintf(`{"error":"User %s is not authorized to access this route (allowed workspaces: %v)"}`, userName, allowWorkspaces)),
		-1,
	)
	return types.ActionContinue
}

// wwwAuthenticateHeader 返回 401 响应的 WWW-Authenticate 头
func wwwAuthenticateHeader(realm string) [][2]string {
	return [][2]string{
		{"Content-Type", "application/json"},
		{"WWW-Authenticate", fmt.Sprintf("Bearer realm=%s", realm)},
	}
}

// responseHeaders 返回标准 JSON 响应头
func responseHeaders() [][2]string {
	return [][2]string{
		{"Content-Type", "application/json"},
	}
}
