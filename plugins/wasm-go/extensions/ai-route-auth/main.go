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

	// 认证头名称。可选，用于强制指定唯一的凭证来源 header。
	// 不配置（或配为默认 "Authorization"）时，插件按优先级自动兼容
	// OpenAI(Authorization: Bearer) 与 Anthropic(x-api-key) 等多种协议。
	authHeaderName string

	// 允许访问的租户-项目列表（从 matchRules 中获取）
	// 替代原 allowWorkspaces
	allowWorkspaceProjects []WorkspaceProject

	// 本路由（=模型）直接授权的 apikey 集合（从 matchRules 中获取）
	// 独立于 workspace/project 的放行通道：命中即放行。
	// 用 set 便于 O(1) 命中查找。
	allowApiKeys map[string]struct{}

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
//	  "allow_workspace_projects": ["*:*", "tenant1:*", "tenant1:project-a,project-b"],
//	  "allow_apikeys": ["sk-aaa", "sk-ccc"]
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

	// Parse allow_apikeys
	// 本路由（=模型）直接授权的 apikey 列表，独立于 workspace/project 的放行通道。
	// 与 allow_workspace_projects 一样，空值是合法语义（仅表示该路由不通过 apikey
	// 维度授权），不在解析阶段报错。
	config.allowApiKeys = make(map[string]struct{})
	allowApiKeysJson := json.Get("allow_apikeys")
	if allowApiKeysJson.IsArray() {
		for _, item := range allowApiKeysJson.Array() {
			key := item.String()
			if key != "" {
				config.allowApiKeys[key] = struct{}{}
			}
		}
	}

	if len(config.allowApiKeys) > 0 {
		log.Debugf("loaded %d directly-authorized apikeys for this route", len(config.allowApiKeys))
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

	// Step 1.1: 命中了 matchRule，但两个授权维度（allow_workspace_projects 与
	// allow_apikeys）都为空。语义为「该路由不允许任何 API Key 访问」(deny all)，直接拒绝。
	// 这里不能放行，否则空配置会变成「对所有人开放」，与预期相反。
	// 注意：只要任一维度非空就不该 deny all，否则只配 allow_apikeys 的路由会被误拒。
	if len(config.allowWorkspaceProjects) == 0 && len(config.allowApiKeys) == 0 {
		log.Warnf("both allow_workspace_projects and allow_apikeys are empty, deny all access to this route")
		return deniedInvalidAPIKey()
	}

	// Step 1.5: 如果 user_apikeys 未配置，则不进行鉴权
	if len(config.apiKeyMapping) == 0 {
		log.Debugf("no authentication configured (apiKeyMapping is empty), skipping authentication")
		return types.ActionContinue
	}

	// Step 2: 提取 API Key（兼容 OpenAI 的 Authorization 与 Anthropic 的 x-api-key）
	apiKey, found := extractCredential(config.authHeaderName)
	if !found {
		log.Warnf("no credential found in any supported auth header")
		return deniedMissingAuthHeader(config.authHeaderName)
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

	// Step 4.5: 模型（=路由）级 apikey 授权：独立优先放行通道。
	// 只要该 apikey 出现在本路由的 allow_apikeys 中，即放行，不再要求 project 匹配。
	// 未命中则回落到 Step 5 的 workspace/project 校验，整体为 OR 语义。
	if _, granted := config.allowApiKeys[apiKey]; granted {
		log.Infof("auth success: user=%s (apikey directly granted on this route)", userName)
		return types.ActionContinue
	}

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
	// 统一对外文案：不暴露 user 是否存在等内部信息，与 key 未授权的情况返回同样的响应。
	log.Warnf("user %q not authorized to access this route", userName)
	return deniedInvalidAPIKey()
}

// ============================================================================
// Helper Functions
// ============================================================================

// anthropicStyleAuthHeaders 定义在未显式配置 auth_header_name 时，按优先级依次
// 检查的 Anthropic / 透传风格认证 header。顺序与主流 ai-proxy 保持一致。
var anthropicStyleAuthHeaders = []string{"x-api-key", "x-authorization", "anthropic-api-key"}

// extractCredential 从请求头中按优先级提取原始 API Key，兼容 OpenAI 与 Anthropic 两种协议：
//   - OpenAI:    Authorization: Bearer <key>
//   - Anthropic: x-api-key: <key>
//
// 优先级（与主流 ai-proxy provider 的默认行为一致）：
//  1. 显式配置的 auth_header_name（非默认值时）——运维可强制指定唯一来源
//  2. x-api-key / x-authorization / anthropic-api-key（Anthropic / 透传风格）
//  3. Authorization: Bearer <key>（OpenAI 风格；无 Bearer 前缀时按原值处理）
//
// 返回 (apiKey, found)。found=false 表示所有候选 header 均缺失或为空。
func extractCredential(configuredHeader string) (string, bool) {
	// 1. 显式配置优先：仅当运维把 auth_header_name 配成非默认值时生效。
	//    此时按该 header 直取原值（不做 Bearer 解析），语义与旧行为保持一致。
	if configuredHeader != "" && !strings.EqualFold(configuredHeader, defaultAuthHeaderName) {
		if v, err := proxywasm.GetHttpRequestHeader(configuredHeader); err == nil {
			if key := strings.TrimSpace(v); key != "" {
				return key, true
			}
		}
	}

	// 2. Anthropic / 透传风格 header
	for _, h := range anthropicStyleAuthHeaders {
		if v, err := proxywasm.GetHttpRequestHeader(h); err == nil {
			if key := strings.TrimSpace(v); key != "" {
				return key, true
			}
		}
	}

	// 3. OpenAI 风格 Authorization: Bearer <key>
	if v, err := proxywasm.GetHttpRequestHeader(defaultAuthHeaderName); err == nil {
		if key := extractBearerToken(v); key != "" {
			return key, true
		}
	}

	return "", false
}

// extractBearerToken 从 Authorization 头中提取 token。
// 兼容 "Bearer <token>" 与直接给出 token 两种写法（与主流 ai-proxy 一致）。
func extractBearerToken(headerValue string) string {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return ""
	}
	if len(headerValue) >= len(bearerPrefix) &&
		strings.EqualFold(headerValue[:len(bearerPrefix)], bearerPrefix) {
		return strings.TrimSpace(headerValue[len(bearerPrefix):])
	}
	return headerValue
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

// wwwAuthenticateHeader 返回 401 响应的 WWW-Authenticate 头
func wwwAuthenticateHeader(realm string) [][2]string {
	return [][2]string{
		{"Content-Type", "application/json"},
		{"WWW-Authenticate", fmt.Sprintf("Bearer realm=%s", realm)},
	}
}
