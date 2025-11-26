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

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelAuth_ValidApiKeyAndModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1", "model-2"],
				"sk-weetime": ["model-3"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with valid API key and model
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
			{"x-higress-llm-model", "model-1"},
		}

		// Call plugin request header processing method
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify no response was sent (request should continue)
		localResponse := host.GetLocalResponse()
		assert.Nil(t, localResponse)
	})
}

func TestModelAuth_ValidApiKeySecondModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1", "model-2"],
				"sk-weetime": ["model-3"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with valid API key and second allowed model
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
			{"x-higress-llm-model", "model-2"},
		}

		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		localResponse := host.GetLocalResponse()
		assert.Nil(t, localResponse)
	})
}

func TestModelAuth_InvalidApiKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1", "model-2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with invalid API key
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-invalid"},
			{"x-higress-llm-model", "model-1"},
		}

		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify unauthorized response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Invalid API key")
	})
}

func TestModelAuth_UnauthorizedModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1", "model-2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with valid API key but unauthorized model
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
			{"x-higress-llm-model", "model-3"},
		}

		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify unauthorized response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Model access denied")
	})
}

func TestModelAuth_MissingAuthorizationHeader(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers without Authorization header
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"x-higress-llm-model", "model-1"},
		}

		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify unauthorized response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Authorization header is required")
	})
}

func TestModelAuth_MissingModelHeader(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers without x-higress-llm-model header
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
		}

		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify unauthorized response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "x-higress-llm-model header is required")
	})
}

func TestModelAuth_InvalidAuthorizationFormat(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with Authorization header without "Bearer " prefix
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "sk-test"},
			{"x-higress-llm-model", "model-1"},
		}

		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify unauthorized response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Invalid Authorization header format")
	})
}

func TestModelAuth_ConfigMissingApiKeyModels(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with empty config
		config := json.RawMessage(`{}`)
		_, status := test.NewTestHost(config)
		// Should fail because api_key_models is required
		require.Equal(t, types.OnPluginStartStatusFailed, status)
	})
}

func TestModelAuth_ConfigEmptyApiKeyModels(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with empty api_key_models
		config := json.RawMessage(`{"api_key_models": {}}`)
		_, status := test.NewTestHost(config)
		// Should fail because at least one API key mapping is required
		require.Equal(t, types.OnPluginStartStatusFailed, status)
	})
}

func TestModelAuth_MultipleApiKeys_FirstKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with multiple API keys
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1"],
				"sk-weetime": ["model-2", "model-3"],
				"sk-audio": ["a1", "a2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test first API key
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
			{"x-higress-llm-model", "model-1"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		assert.Nil(t, host.GetLocalResponse())
	})
}

func TestModelAuth_MultipleApiKeys_SecondKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with multiple API keys
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1"],
				"sk-weetime": ["model-2", "model-3"],
				"sk-audio": ["a1", "a2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test second API key
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-weetime"},
			{"x-higress-llm-model", "model-2"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		assert.Nil(t, host.GetLocalResponse())
	})
}

func TestModelAuth_MultipleApiKeys_ThirdKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with multiple API keys
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-test": ["model-1"],
				"sk-weetime": ["model-2", "model-3"],
				"sk-audio": ["a1", "a2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test third API key
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-audio"},
			{"x-higress-llm-model", "a1"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		assert.Nil(t, host.GetLocalResponse())
	})
}

func TestModelAuth_RealWorldExample_AllowedModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with real-world configuration
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-weetime": ["gen-studio-Qwen2.5-0.5B-Instruct-bbbb", "gen-studio-Qwen2.5-0.5B-Instruct-aaa"],
				"sk-test": ["gen-studio-Qwen2.5-0.5B-Instruct-bbbb"],
				"sk-audio": ["a1", "a2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test sk-test with allowed model
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
			{"x-higress-llm-model", "gen-studio-Qwen2.5-0.5B-Instruct-bbbb"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		assert.Nil(t, host.GetLocalResponse())
	})
}

func TestModelAuth_RealWorldExample_DeniedModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with real-world configuration
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-weetime": ["gen-studio-Qwen2.5-0.5B-Instruct-bbbb", "gen-studio-Qwen2.5-0.5B-Instruct-aaa"],
				"sk-test": ["gen-studio-Qwen2.5-0.5B-Instruct-bbbb"],
				"sk-audio": ["a1", "a2"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test sk-test with model it doesn't have access to
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-test"},
			{"x-higress-llm-model", "gen-studio-Qwen2.5-0.5B-Instruct-aaa"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Model access denied")
	})
}

func TestModelAuth_CustomHeaderNames(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with custom header names
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-custom": ["model-x"]
			},
			"auth_header_name": "X-API-Key",
			"model_header_name": "X-Model-Name"
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test with custom headers
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"X-API-Key", "Bearer sk-custom"},
			{"X-Model-Name", "model-x"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		assert.Nil(t, host.GetLocalResponse())
	})
}

func TestModelAuth_CustomHeaderNames_MissingAuthHeader(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with custom header names
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-custom": ["model-x"]
			},
			"auth_header_name": "X-API-Key",
			"model_header_name": "X-Model-Name"
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test with missing custom auth header
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"X-Model-Name", "model-x"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "X-API-Key header is required")
	})
}

func TestModelAuth_CustomHeaderNames_MissingModelHeader(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with custom header names
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-custom": ["model-x"]
			},
			"auth_header_name": "X-API-Key",
			"model_header_name": "X-Model-Name"
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test with missing custom model header
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"X-API-Key", "Bearer sk-custom"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "X-Model-Name header is required")
	})
}

func TestModelAuth_DefaultHeaderNames(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host without specifying header names (should use defaults)
		config := json.RawMessage(`{
			"api_key_models": {
				"sk-default": ["model-a"]
			}
		}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Test with default headers
		headers := [][2]string{
			{":method", "POST"},
			{":path", "/v1/chat/completions"},
			{":authority", "test.com"},
			{"Authorization", "Bearer sk-default"},
			{"x-higress-llm-model", "model-a"},
		}
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)
		assert.Nil(t, host.GetLocalResponse())
	})
}
