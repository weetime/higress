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
	"testing"

	"github.com/alibaba/higress/plugins/wasm-go/pkg/mcp/consts"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const aggregatedRoute = "mcp-aggregated-route"

// separator is the tool-name separator. It is spelled out literally here so a
// change to the shared constant cannot silently alter the wire format that
// clients and the mcp-server toolSet already depend on.
const separator = "___"

func TestSeparatorMatchesCode(t *testing.T) {
	require.Equal(t, consts.ToolSetNameSplitter, separator)
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// routingConfig is the shape Higress serializes when `defaultConfig` carries the
// routing table and a `matchRules` entry enables the filter on one route.
var routingConfig = mustJSON(map[string]any{
	"servers": []map[string]any{
		{"name": "demo-alpha", "path": "/mcp-servers/demo-alpha"},
		{"name": "demo-beta", "domain": "beta.example.com", "path": "/mcp-servers/demo-beta"},
	},
	"_rules_": []map[string]any{
		{"_match_route_": []string{aggregatedRoute}, "enable": true},
	},
})

// enabledWithoutGlobalConfig reproduces what reaches the Wasm VM when the
// WasmPlugin sets `defaultConfigDisable: true`: `defaultConfig` is dropped, so
// `_rules_` is the only top-level key and the rule matcher never parses a global
// config. The filter cannot route without the `servers` table, so a rule that
// asks to be enabled must fail loudly rather than silently do nothing.
var enabledWithoutGlobalConfig = mustJSON(map[string]any{
	"_rules_": []map[string]any{
		{"_match_route_": []string{aggregatedRoute}, "enable": true},
	},
})

// disabledWithoutGlobalConfig is the same shape but the rule does not ask to be
// enabled, which is a legitimate no-op rather than a misconfiguration.
var disabledWithoutGlobalConfig = mustJSON(map[string]any{
	"_rules_": []map[string]any{
		{"_match_route_": []string{aggregatedRoute}, "enable": false},
	},
})

func toolCallBody(toolName string) []byte {
	return mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": map[string]any{},
		},
	})
}

func headerValue(headers [][2]string, name string) string {
	for _, h := range headers {
		if h[0] == name {
			return h[1]
		}
	}
	return ""
}

// callTool drives one tools/call request through the filter on the aggregated
// route and returns the resulting request headers and body.
func callTool(t *testing.T, config json.RawMessage, toolName string) ([][2]string, []byte) {
	t.Helper()
	host, status := test.NewTestHost(config)
	defer host.Reset()
	require.Equal(t, types.OnPluginStartStatusOK, status)

	host.SetRouteName(aggregatedRoute)
	host.CallOnHttpRequestHeaders([][2]string{
		{":authority", "mcp.example.com"},
		{":method", "POST"},
		{":path", "/mcp-servers/demo-all"},
		{"content-type", "application/json"},
	})
	host.CallOnHttpRequestBody(toolCallBody(toolName))

	return host.GetRequestHeaders(), host.GetRequestBody()
}

func TestConfigParsing(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("routing table in defaultConfig is accepted", func(t *testing.T) {
			host, status := test.NewTestHost(routingConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
		})

		// Regression test: before this was fixed the plugin silently set
		// enable=false and every tools/call fell through unrouted.
		t.Run("enabled rule without global config is rejected", func(t *testing.T) {
			host, status := test.NewTestHost(enabledWithoutGlobalConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})

		t.Run("disabled rule without global config is accepted", func(t *testing.T) {
			host, status := test.NewTestHost(disabledWithoutGlobalConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
		})

		t.Run("server without name is rejected", func(t *testing.T) {
			config := mustJSON(map[string]any{
				"servers": []map[string]any{{"path": "/mcp-servers/demo-alpha"}},
				"_rules_": []map[string]any{
					{"_match_route_": []string{aggregatedRoute}, "enable": true},
				},
			})
			host, status := test.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})

		t.Run("server without path is rejected", func(t *testing.T) {
			config := mustJSON(map[string]any{
				"servers": []map[string]any{{"name": "demo-alpha"}},
				"_rules_": []map[string]any{
					{"_match_route_": []string{aggregatedRoute}, "enable": true},
				},
			})
			host, status := test.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})
	})
}

func TestToolCallRouting(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("prefixed tool is rerouted and the prefix is stripped", func(t *testing.T) {
			headers, body := callTool(t, routingConfig, "demo-alpha"+separator+"alpha-version")

			require.Equal(t, "/mcp-servers/demo-alpha", headerValue(headers, ":path"))
			require.Equal(t, "true", headerValue(headers, "x-envoy-internal-route"))
			// No domain configured for demo-alpha, so :authority is preserved.
			require.Equal(t, "mcp.example.com", headerValue(headers, ":authority"))
			require.Equal(t, "alpha-version", gjson.GetBytes(body, "params.name").String())
		})

		t.Run("configured domain replaces the authority", func(t *testing.T) {
			headers, body := callTool(t, routingConfig, "demo-beta"+separator+"beta-deviceplugins")

			require.Equal(t, "/mcp-servers/demo-beta", headerValue(headers, ":path"))
			require.Equal(t, "beta.example.com", headerValue(headers, ":authority"))
			require.Equal(t, "beta-deviceplugins", gjson.GetBytes(body, "params.name").String())
		})

		t.Run("tool without a prefix is left untouched", func(t *testing.T) {
			headers, body := callTool(t, routingConfig, "alpha-version")

			require.Equal(t, "/mcp-servers/demo-all", headerValue(headers, ":path"))
			require.Equal(t, "alpha-version", gjson.GetBytes(body, "params.name").String())
		})

		t.Run("unknown server prefix is left untouched", func(t *testing.T) {
			headers, body := callTool(t, routingConfig, "nonexistent"+separator+"alpha-version")

			require.Equal(t, "/mcp-servers/demo-all", headerValue(headers, ":path"))
			require.Equal(t, "nonexistent"+separator+"alpha-version", gjson.GetBytes(body, "params.name").String())
		})

		// A slash is not the separator, so a slash-separated name is treated as
		// unprefixed and passes through untouched.
		t.Run("slash is not treated as a separator", func(t *testing.T) {
			headers, body := callTool(t, routingConfig, "demo-alpha/alpha-version")

			require.Equal(t, "/mcp-servers/demo-all", headerValue(headers, ":path"))
			require.Equal(t, "demo-alpha/alpha-version", gjson.GetBytes(body, "params.name").String())
		})
	})
}

func TestNonToolCallIsUntouched(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("tools/list passes through", func(t *testing.T) {
			host, status := test.NewTestHost(routingConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.SetRouteName(aggregatedRoute)
			host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "mcp.example.com"},
				{":method", "POST"},
				{":path", "/mcp-servers/demo-all"},
				{"content-type", "application/json"},
			})
			action := host.CallOnHttpRequestBody(mustJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/list",
			}))

			require.Equal(t, types.ActionContinue, action)
			require.Equal(t, "/mcp-servers/demo-all", headerValue(host.GetRequestHeaders(), ":path"))
		})
	})
}
