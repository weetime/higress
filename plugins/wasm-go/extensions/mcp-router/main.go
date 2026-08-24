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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/alibaba/higress/plugins/wasm-go/pkg/mcp"
	"github.com/alibaba/higress/plugins/wasm-go/pkg/mcp/consts"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func main() {}

func init() {
	mcp.LoadMCPFilter(
		mcp.FilterName("mcp-router"),
		mcp.SetConfigOverrideParser(ParseGlobalConfig, ParseOverrideConfig),
		mcp.SetToolCallRequestFilter(ProcessRequest),
	)
	mcp.InitMCPFilter()
}

// ServerConfig represents the routing configuration for a single MCP server
type ServerConfig struct {
	Name   string `json:"name"`
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path"`
}

// McpRouterGlobalConfig represents the global configuration for the mcp-router filter
type McpRouterGlobalConfig struct {
	Servers []ServerConfig `json:"servers"`
}

type McpRouterConfig struct {
	global *McpRouterGlobalConfig
	enable bool
}

func ParseGlobalConfig(configBytes []byte, globalConfig *any) error {
	var config McpRouterGlobalConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return fmt.Errorf("failed to parse mcp-router config: %v", err)
	}

	for i, server := range config.Servers {
		if server.Name == "" {
			return fmt.Errorf("servers[%d].name is required", i)
		}
		if server.Path == "" {
			return fmt.Errorf("servers[%d].path is required (server %q)", i, server.Name)
		}
	}

	log.Infof("Parsed mcp-router config with %d servers", len(config.Servers))
	for _, server := range config.Servers {
		log.Debugf("Server: %s -> %s%s", server.Name, server.Domain, server.Path)
	}

	*globalConfig = config
	return nil
}

func ParseOverrideConfig(configBytes []byte, globalConfig any, ruleConfig *any) error {
	var config McpRouterConfig
	enable := gjson.GetBytes(configBytes, "enable").Bool()
	if globalConfig == nil {
		// The `servers` routing table only ever lives in the global config, so without it
		// this filter cannot route anything. The usual cause is `defaultConfigDisable: true`
		// on the WasmPlugin: Higress then omits `defaultConfig` from the wasm configuration,
		// leaving `_rules_` as the only top-level key, and the rule matcher skips global
		// config parsing entirely. Fail loudly instead of silently accepting a rule that
		// asked to be enabled.
		if enable {
			return errors.New("mcp-router is enabled on this rule but no global config was parsed; " +
				"declare the `servers` routing table under `defaultConfig` and do not set `defaultConfigDisable: true`")
		}
		config.global = &McpRouterGlobalConfig{}
		config.enable = false
		*ruleConfig = config
		return nil
	}
	parent, ok := globalConfig.(McpRouterGlobalConfig)
	if !ok {
		return fmt.Errorf("invalid globalConfig: %v", globalConfig)
	}
	config.global = &parent
	config.enable = enable
	*ruleConfig = config
	return nil
}

func ProcessRequest(context wrapper.HttpContext, config any, toolName string, toolArgs gjson.Result, rawBody []byte) types.Action {
	routerConfig, ok := config.(McpRouterConfig)
	if !ok {
		log.Errorf("Invalid config type for mcp-router")
		return types.ActionContinue
	}
	if !routerConfig.enable {
		return types.ActionContinue
	}
	// Extract server name from tool name (format: "${serverName}___${toolName}")
	parts := strings.SplitN(toolName, consts.ToolSetNameSplitter, 2)
	if len(parts) != 2 {
		log.Debugf("Tool name '%s' does not contain server prefix, continuing without routing", toolName)
		return types.ActionContinue
	}

	serverName := parts[0]
	actualToolName := parts[1]

	log.Debugf("Routing tool call: server=%s, tool=%s", serverName, actualToolName)

	// Find the server configuration
	var targetServer *ServerConfig
	for _, server := range routerConfig.global.Servers {
		if server.Name == serverName {
			targetServer = &server
			break
		}
	}

	if targetServer == nil {
		log.Warnf("No routing configuration found for server '%s'", serverName)
		return types.ActionContinue
	}

	log.Infof("Routing to server '%s': domain=[%s], path=[%s]", serverName, targetServer.Domain, targetServer.Path)

	// Modify the :authority header (domain) if it's configured
	if targetServer.Domain != "" {
		if err := proxywasm.ReplaceHttpRequestHeader(":authority", targetServer.Domain); err != nil {
			log.Errorf("Failed to set :authority header to '%s': %v", targetServer.Domain, err)
			return types.ActionContinue
		}
	}

	proxywasm.ReplaceHttpRequestHeader("x-envoy-internal-route", "true")

	// Modify the :path header
	if err := proxywasm.ReplaceHttpRequestHeader(":path", targetServer.Path); err != nil {
		log.Errorf("Failed to set :path header to '%s': %v", targetServer.Path, err)
		return types.ActionContinue
	}

	// Create a new JSON with the modified tool name
	modifiedBody, err := sjson.SetBytes(rawBody, "params.name", actualToolName)
	if err != nil {
		log.Errorf("Failed to modify tool name, body: %s, err: %v", rawBody, err)
		return types.ActionContinue
	}
	// Replace the request body
	if err := proxywasm.ReplaceHttpRequestBody([]byte(modifiedBody)); err != nil {
		log.Errorf("Failed to replace request body: %v", err)
		return types.ActionContinue
	}

	log.Infof("Successfully routed request for tool '%s' to server '%s'. New tool name is '%s'.",
		toolName, serverName, actualToolName)
	return types.ActionContinue
}
