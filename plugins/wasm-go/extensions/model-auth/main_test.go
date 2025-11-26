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

func TestModelAuth_ValidKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{"api_key": "test-secret-key-123"}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with valid API key
		headers := [][2]string{
			{":method", "GET"},
			{":path", "/api/test"},
			{":authority", "test.com"},
			{"X-Model-Auth-Key", "test-secret-key-123"},
		}

		// Call plugin request header processing method
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify no response was sent (request should continue)
		localResponse := host.GetLocalResponse()
		assert.Nil(t, localResponse)
	})
}

func TestModelAuth_InvalidKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{"api_key": "test-secret-key-123"}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers with invalid API key
		headers := [][2]string{
			{":method", "GET"},
			{":path", "/api/test"},
			{":authority", "test.com"},
			{"X-Model-Auth-Key", "wrong-key"},
		}

		// Call plugin request header processing method
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify forbidden response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(403), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Forbidden: Invalid API key")
	})
}

func TestModelAuth_MissingKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with config
		config := json.RawMessage(`{"api_key": "test-secret-key-123"}`)
		host, status := test.NewTestHost(config)
		require.Equal(t, types.OnPluginStartStatusOK, status)
		defer host.Reset()

		// Set request headers without API key
		headers := [][2]string{
			{":method", "GET"},
			{":path", "/api/test"},
			{":authority", "test.com"},
		}

		// Call plugin request header processing method
		action := host.CallOnHttpRequestHeaders(headers)
		require.Equal(t, types.ActionContinue, action)

		// Verify unauthorized response was sent
		localResponse := host.GetLocalResponse()
		require.NotNil(t, localResponse)
		assert.Equal(t, uint32(401), localResponse.StatusCode)
		assert.Contains(t, string(localResponse.Data), "Unauthorized: X-Model-Auth-Key header is required")
	})
}

func TestModelAuth_ConfigMissingApiKey(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// Create test host with empty config
		config := json.RawMessage(`{}`)
		_, status := test.NewTestHost(config)
		// Should fail because api_key is required
		require.Equal(t, types.OnPluginStartStatusFailed, status)
	})
}

