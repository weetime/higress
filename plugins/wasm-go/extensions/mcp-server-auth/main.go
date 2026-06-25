// Copyright (c) 2023 Alibaba Group Holding Ltd.
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

var (
	protectionSpace = "MSE Gateway" // 认证失败时，返回响应头 WWW-Authenticate: Key realm=MSE Gateway
)

func main() {}

func init() {
	wrapper.SetCtx(
		"mcp-server-auth", // middleware name
		wrapper.ParseOverrideConfigBy(parseGlobalConfig, parseOverrideRuleConfig),
		wrapper.ProcessRequestHeadersBy(onHttpRequestHeaders),
	)
}

// @Name mcp-server-auth
// @Category auth
// @Phase AUTHN
// @Priority 320
// @Title zh-CN MCP Server Auth
// @Description zh-CN 最小化鉴权 demo 插件，从 HTTP 请求头解析 token，并在路由级用 allow 列表直接校验合法 token。
// @Description en-US A minimal auth demo plugin that parses a token from HTTP request headers and validates it against a route-level allow list of tokens.
// @IconUrl https://img.alicdn.com/imgextra/i4/O1CN01BPFGlT1pGZ2VDLgaH_!!6000000005333-2-tps-42-42.png
// @Version 1.0.0
//
// @Contact.name Higress Team
// @Contact.url http://higress.io/
// @Contact.email admin@higress.io
//
// @Example
// keys:
//   - Authorization
// @End
type McpAuthConfig struct {
	// @Title token 的来源请求头名称列表
	// @Title en-US The name of the source header of the token
	// @Description token 的来源请求头名称（支持 Authorization: Bearer <token> 形式）。
	// @Description en-US The name of the source request header of the token (supports the Authorization: Bearer <token> form).
	// @Scope GLOBAL
	Keys []string `yaml:"keys"` // token auth header names

	// @Title 授权访问的合法 token 列表
	// @Title en-US Allowed tokens
	// @Description 对于匹配的路由/域名，允许访问的合法 token 列表。
	// @Description en-US List of allowed tokens for matched routes/domains.
	allow []string `yaml:"allow"`
}

func parseGlobalConfig(json gjson.Result, global *McpAuthConfig, log log.Log) error {
	log.Debug("global config")

	// keys
	names := json.Get("keys")
	if !names.Exists() {
		return errors.New("keys is required")
	}
	if len(names.Array()) == 0 {
		return errors.New("keys cannot be empty")
	}

	for _, name := range names.Array() {
		global.Keys = append(global.Keys, name.String())
	}
	return nil
}

func parseOverrideRuleConfig(json gjson.Result, global McpAuthConfig, config *McpAuthConfig, log log.Log) error {
	log.Debug("domain/route config")

	*config = global

	allow := json.Get("allow")
	if !allow.Exists() {
		return errors.New("allow is required")
	}
	if len(allow.Array()) == 0 {
		return errors.New("allow cannot be empty")
	}

	for _, item := range allow.Array() {
		config.allow = append(config.allow, item.String())
	}

	return nil
}

// mcp-server-auth 插件认证逻辑（最小化）：
// - 从配置的请求头中解析 token（支持去掉 Bearer 前缀）
// - 直接在路由级 allow 列表中校验该 token，命中放行，否则拒绝
func onHttpRequestHeaders(ctx wrapper.HttpContext, config McpAuthConfig, log log.Log) types.Action {
	// 仅对配置了 allow 的路由/域名强制鉴权。
	// 未命中任何 matchRule 的请求会回退到全局配置（allow 为空），此处直接放行，
	// 避免插件在未绑定的路由上误拦截。
	if len(config.allow) == 0 {
		log.Debug("authorization is not required for this route")
		return types.ActionContinue
	}

	// 从 header 中获取 tokens 信息
	var tokens []string
	for _, key := range config.Keys {
		value, err := proxywasm.GetHttpRequestHeader(key)
		if err != nil || value == "" {
			continue
		}
		if strings.EqualFold(key, "Authorization") {
			// 标准 Authorization 头：必须携带 Bearer scheme（RFC 6750）。
			// 缺少 scheme 或非 Bearer 的值视为无效凭证，不予收集（最终触发 401）。
			token, ok := parseBearerToken(value)
			if !ok {
				log.Warn("Authorization header without a valid Bearer scheme, ignored")
				continue
			}
			tokens = append(tokens, token)
			continue
		}
		// 自定义头（x-api-key、ak 等）：取原始头值作为 token
		tokens = append(tokens, value)
	}

	if len(tokens) > 1 {
		return deniedMultiKeyAuthData()
	} else if len(tokens) <= 0 {
		return deniedNoKeyAuthData()
	}

	// 路由/域名级授权：token 必须在该路由的 allow 列表中
	if !contains(config.allow, tokens[0]) {
		log.Warnf("token %q is not allowed", tokens[0])
		return deniedUnauthorizedToken()
	}

	log.Info("request authenticated")
	return types.ActionContinue
}

func deniedMultiKeyAuthData() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusUnauthorized, "mcp-server-auth.multi_key", WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. Multi Key Authentication information found."), -1)
	return types.ActionContinue
}

func deniedNoKeyAuthData() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusUnauthorized, "mcp-server-auth.no_key", WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. No Key Authentication information found."), -1)
	return types.ActionContinue
}

func deniedUnauthorizedToken() types.Action {
	_ = proxywasm.SendHttpResponseWithDetail(http.StatusForbidden, "mcp-server-auth.unauthorized", WWWAuthenticateHeader(protectionSpace),
		[]byte("Request denied by MCP Server Auth check. Invalid token."), -1)
	return types.ActionContinue
}

// parseBearerToken 解析 `Authorization: Bearer <token>` 形式的头值（scheme 大小写不敏感）。
// 仅当值带有合法的 Bearer scheme 时返回 (token, true)；否则返回 ("", false)。
func parseBearerToken(value string) (string, bool) {
	const prefix = "Bearer "
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		token := strings.TrimSpace(value[len(prefix):])
		if token != "" {
			return token, true
		}
	}
	return "", false
}

func contains(arr []string, item string) bool {
	for _, i := range arr {
		if i == item {
			return true
		}
	}
	return false
}

func WWWAuthenticateHeader(realm string) [][2]string {
	return [][2]string{
		{"WWW-Authenticate", fmt.Sprintf("Key realm=%s", realm)},
	}
}
