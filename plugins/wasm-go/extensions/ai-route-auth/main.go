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

// WorkspaceProject 表示一个租户及其下的项目列表
// 序列化格式: "workspaceName:project1,project2" 或 "*:*"
type WorkspaceProject struct {
	WorkspaceName string
	ProjectNames  []string
}

// RouteAuthConfig 路由认证配置
type RouteAuthConfig struct {
	// 租户到用户的映射 (workspace -> []users)
	workspaceUsers map[string][]string

	// 项目到用户的映射 (project -> []users)
	projectUsers map[string][]string

	// API Key 到用户的映射 (apiKey -> username)
	// 从 user_apikeys 转换而来，用于快速查找
	apiKeyMapping map[string]string

	// 认证头名称，默认 "Authorization"
	authHeaderName string

	// 允许访问的租户-项目列表（从 matchRules 中获取）
	// 替代原 allowWorkspaces
	allowWorkspaceProjects []WorkspaceProject

	// ruleConfigured 表示该配置来自匹配到的 matchRule（而非 defaultConfig）。
	// 用于区分两种 allowWorkspaceProjects 为空的情况：
	//   - 未匹配到任何 matchRule（用的是 defaultConfig）   -> 跳过鉴权、放行
	//   - 匹配到了 matchRule 但 allow 列表为空（配置异常）  -> 拒绝该路由
	ruleConfigured bool
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
//	  "project_users": {
//	    "project-a": ["admin", "user1"]
//	  },
//	  "user_apikeys": {
//	    "admin": ["sk-aaa", "ak-bbb"],
//	    "username2": ["sk-ccc"]
//	  },
//	  "auth_header_name": "Authorization"
//	}
func parseConfig(json gjson.Result, config *RouteAuthConfig, log log.Log) error {
	config.workspaceUsers = make(map[string][]string)
	config.projectUsers = make(map[string][]string)
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
		log.Infof("workspace_users not configured")
	}

	// Parse project_users (optional)
	// 项目级别的用户列表，用于精确到项目的鉴权
	projectUsersJson := json.Get("project_users")
	if projectUsersJson.Exists() {
		projectUsersJson.ForEach(func(key, value gjson.Result) bool {
			project := key.String()
			users := make([]string, 0)
			if value.IsArray() {
				for _, user := range value.Array() {
					users = append(users, user.String())
				}
			}
			config.projectUsers[project] = users
			log.Debugf("loaded project %q with %d users: %v", project, len(users), users)
			return true
		})
	} else {
		log.Infof("project_users not configured")
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

	if len(config.apiKeyMapping) > 0 || len(config.workspaceUsers) > 0 || len(config.projectUsers) > 0 {
		log.Infof("loaded %d API keys, %d workspaces, %d projects",
			len(config.apiKeyMapping), len(config.workspaceUsers), len(config.projectUsers))
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
//	  "allow_workspace_projects": ["*:*", "tenant1:*", "tenant1:project-a,project-b"]
//	}
func parseRuleConfig(json gjson.Result, global RouteAuthConfig, config *RouteAuthConfig, log log.Log) error {
	// 继承全局配置
	*config = global

	// 标记该配置来自匹配到的 matchRule，用于在请求处理时区分
	// 「未匹配到 matchRule（放行）」与「匹配到但 allow 列表为空（拒绝）」。
	config.ruleConfigured = true

	// Parse allow_workspace_projects
	// 注意：这里【不再】因为字段缺失或为空而返回 error。
	// wrapper.ParseOverrideConfigBy 下，任意一条 matchRule 解析失败都会导致整个插件
	// 配置加载失败；配合 failStrategy=FAIL_CLOSE，会使网关上【所有】路由全部返回 403。
	// 空列表是一个合法语义：表示该路由不允许任何 API Key 访问（deny all），
	// 这一拒绝逻辑在 onHttpRequestHeaders 中处理，而不是在解析阶段报错。
	config.allowWorkspaceProjects = make([]WorkspaceProject, 0)
	allowWPJson := json.Get("allow_workspace_projects")
	if allowWPJson.IsArray() {
		for _, item := range allowWPJson.Array() {
			wp := parseWorkspaceProjectString(item.String())
			if wp != nil {
				config.allowWorkspaceProjects = append(config.allowWorkspaceProjects, *wp)
			}
		}
	}

	if len(config.allowWorkspaceProjects) == 0 {
		log.Warnf("allow_workspace_projects is empty; this route will deny all API keys")
	} else {
		log.Debugf("loaded allow_workspace_projects: %v", config.allowWorkspaceProjects)
	}

	// rule_name 字段仅作为配置标识，插件逻辑中不需要使用

	return nil
}

// parseWorkspaceProjectString 解析 "workspace:project1,project2" 格式的字符串
func parseWorkspaceProjectString(s string) *WorkspaceProject {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	return &WorkspaceProject{
		WorkspaceName: parts[0],
		ProjectNames:  strings.Split(parts[1], ","),
	}
}

// ============================================================================
// Request Processing
// ============================================================================

// onHttpRequestHeaders 处理 HTTP 请求头，进行路由权限验证
func onHttpRequestHeaders(ctx wrapper.HttpContext, config RouteAuthConfig, log log.Log) types.Action {
	// Step 1: 未匹配到任何 matchRule（用的是 defaultConfig，没有 allow_workspace_projects）
	// 这种情况下跳过处理，放行（鉴权只在 matchRules 命中的路由上生效）。
	if !config.ruleConfigured {
		log.Debugf("no matched rule, skipping")
		return types.ActionContinue
	}

	// Step 1.1: 命中了 matchRule，但 allow_workspace_projects 为空。
	// 语义为「该路由不允许任何 API Key 访问」(deny all)，直接拒绝。
	// 这里不能放行，否则空配置会变成「对所有人开放」，与预期相反。
	if len(config.allowWorkspaceProjects) == 0 {
		log.Warnf("allow_workspace_projects is empty, deny all access to this route")
		return deniedInvalidAPIKey()
	}

	// Step 1.5: 如果 user_apikeys 未配置，则不进行鉴权
	if len(config.apiKeyMapping) == 0 {
		log.Debugf("no authentication configured (apiKeyMapping is empty), skipping authentication")
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
	consumerValue := userName + "/" + apiKeySuffix
	_ = proxywasm.ReplaceHttpRequestHeader("x-api-key-name", consumerValue)
	_ = proxywasm.ReplaceHttpRequestHeader("x-mse-consumer", consumerValue)
	log.Debugf("set consumer header: user=%s", userName)

	// Step 5: 权限验证（三级鉴权）
	for _, wp := range config.allowWorkspaceProjects {
		// 情况A: WorkspaceName 为 "*"，公开访问，所有 API Key 用户都能访问
		if wp.WorkspaceName == "*" {
			log.Infof("auth success: user=%s (public wildcard)", userName)
			return types.ActionContinue
		}

		// 情况B: ProjectNames 包含 "*"，该 Workspace 下所有用户都可以访问
		if containsWildcard(wp.ProjectNames) {
			users, exists := config.workspaceUsers[wp.WorkspaceName]
			if exists && contains(users, userName) {
				log.Infof("auth success: user=%s, workspace=%s (all projects)", userName, wp.WorkspaceName)
				return types.ActionContinue
			}
			continue
		}

		// 情况C: 指定了具体的 ProjectNames，只有这些 Project 中的用户才能访问
		for _, projectName := range wp.ProjectNames {
			users, exists := config.projectUsers[projectName]
			if exists && contains(users, userName) {
				log.Infof("auth success: user=%s, workspace=%s, project=%s", userName, wp.WorkspaceName, projectName)
				return types.ActionContinue
			}
		}
	}

	// 用户不在任何允许的租户/项目中
	log.Warnf("user %q not authorized to access this route", userName)
	return deniedAccessDenied(userName)
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

// containsWildcard 检查切片中是否包含通配符 "*"
func containsWildcard(slice []string) bool {
	for _, s := range slice {
		if s == "*" {
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

// deniedInvalidAPIKey 当 API key 不允许访问此路由时返回 403
func deniedInvalidAPIKey() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusForbidden,
		pluginName+".access_denied",
		wwwAuthenticateHeader(protectionSpace),
		[]byte(`{"error":"API key is not allowed to access this route"}`),
		-1,
	)
	return types.ActionContinue
}

// deniedAccessDenied 当用户无权访问路由时返回 403
func deniedAccessDenied(userName string) types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusForbidden,
		pluginName+".access_denied",
		responseHeaders(),
		[]byte(fmt.Sprintf(`{"error":"User %s is not authorized to access this route"}`, userName)),
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
