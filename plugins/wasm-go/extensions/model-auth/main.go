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
	"net/http"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

func main() {}

func init() {
	wrapper.SetCtx(
		"model-auth",
		wrapper.ParseConfigBy(parseConfig),
		wrapper.ProcessRequestHeadersBy(onHttpRequestHeaders),
	)
}

// ModelAuthConfig stores the mapping of API keys to their allowed models
type ModelAuthConfig struct {
	// apiKeyModels is a map where key is the API key and value is a slice of allowed model names
	apiKeyModels map[string][]string
	// authHeaderName is the name of the header containing the API key (default: "Authorization")
	authHeaderName string
	// modelHeaderName is the name of the header containing the model name (default: "x-higress-llm-model")
	modelHeaderName string
}

// parseConfig parses the plugin configuration
// Expected configuration format:
//
//	{
//	  "api_key_models": {
//	    "sk-test": ["gen-studio-Qwen2.5-0.5B-Instruct-bbbb"],
//	    "sk-weetime": ["gen-studio-Qwen2.5-0.5B-Instruct-bbbb", "gen-studio-Qwen2.5-0.5B-Instruct-aaa"],
//	    "sk-audio": ["a1", "a2"]
//	  },
//	  "auth_header_name": "Authorization",  // optional, default: "Authorization"
//	  "model_header_name": "x-higress-llm-model"  // optional, default: "x-higress-llm-model"
//	}
func parseConfig(json gjson.Result, config *ModelAuthConfig, log log.Log) error {
	config.apiKeyModels = make(map[string][]string)

	// Parse auth_header_name (optional, default: "Authorization")
	authHeaderName := json.Get("auth_header_name").String()
	if authHeaderName == "" {
		authHeaderName = "Authorization"
	}
	config.authHeaderName = authHeaderName

	// Parse model_header_name (optional, default: "x-higress-llm-model")
	modelHeaderName := json.Get("model_header_name").String()
	if modelHeaderName == "" {
		modelHeaderName = "x-higress-llm-model"
	}
	config.modelHeaderName = modelHeaderName

	log.Infof("Using auth header: '%s', model header: '%s'", config.authHeaderName, config.modelHeaderName)

	apiKeyModelsJson := json.Get("api_key_models")
	if !apiKeyModelsJson.Exists() {
		return errors.New("api_key_models is required in configuration")
	}

	// Parse the api_key_models object
	apiKeyModelsJson.ForEach(func(key, value gjson.Result) bool {
		apiKey := key.String()
		models := []string{}

		// Parse the array of models for this API key
		if value.IsArray() {
			for _, model := range value.Array() {
				models = append(models, model.String())
			}
		}

		config.apiKeyModels[apiKey] = models
		log.Infof("Loaded API key '%s' with %d allowed models", apiKey, len(models))
		return true
	})

	if len(config.apiKeyModels) == 0 {
		return errors.New("at least one API key mapping is required")
	}

	return nil
}

// onHttpRequestHeaders validates the API key and model from request headers
func onHttpRequestHeaders(ctx wrapper.HttpContext, config ModelAuthConfig, log log.Log) types.Action {
	// Step 1: Extract API key from configured auth header
	authHeader, err := proxywasm.GetHttpRequestHeader(config.authHeaderName)
	if err != nil || authHeader == "" {
		log.Warnf("%s header is missing", config.authHeaderName)
		sendUnauthorizedResponse(config.authHeaderName + " header is required")
		return types.ActionContinue
	}

	// Extract API key based on header type
	var apiKey string
	if config.authHeaderName == "Authorization" {
		// For Authorization header, expect Bearer token format
		apiKey = extractBearerToken(authHeader)
		if apiKey == "" {
			log.Warnf("Invalid %s header format, Bearer token not found", config.authHeaderName)
			sendUnauthorizedResponse("Invalid " + config.authHeaderName + " header format. Expected: Authorization: Bearer <apiKey>")
			return types.ActionContinue
		}
	} else {
		// For other headers (e.g., x-api-key), use the value directly
		apiKey = strings.TrimSpace(authHeader)
		if apiKey == "" {
			log.Warnf("Invalid %s header: value is empty", config.authHeaderName)
			sendUnauthorizedResponse(config.authHeaderName + " header value cannot be empty")
			return types.ActionContinue
		}
	}

	// Step 2: Extract model name from configured model header
	modelName, err := proxywasm.GetHttpRequestHeader(config.modelHeaderName)
	if err != nil || modelName == "" {
		log.Warnf("%s header is missing", config.modelHeaderName)
		sendUnauthorizedResponse(config.modelHeaderName + " header is required")
		return types.ActionContinue
	}

	// Step 3: Validate API key exists in configuration
	allowedModels, exists := config.apiKeyModels[apiKey]
	if !exists {
		log.Warnf("API key not found: %s", apiKey)
		sendUnauthorizedResponse("Invalid API key")
		return types.ActionContinue
	}

	// Step 4: Validate model is in the allowed list for this API key
	if !contains(allowedModels, modelName) {
		log.Warnf("Model '%s' not allowed for API key '%s'", modelName, apiKey)
		sendUnauthorizedResponse("Model access denied for this API key")
		return types.ActionContinue
	}

	// Success: API key and model are valid
	log.Infof("Authorization successful: API key '%s' accessing model '%s'", apiKey, modelName)
	return types.ActionContinue
}

// extractBearerToken extracts the token from "Bearer <token>" format
func extractBearerToken(authHeader string) string {
	const bearerPrefix = "Bearer "
	if strings.HasPrefix(authHeader, bearerPrefix) {
		return strings.TrimSpace(authHeader[len(bearerPrefix):])
	}
	return ""
}

// contains checks if a string slice contains a specific string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// sendUnauthorizedResponse sends a 401 Unauthorized response
func sendUnauthorizedResponse(message string) {
	proxywasm.SendHttpResponseWithDetail(
		http.StatusUnauthorized,
		"model-auth.unauthorized",
		[][2]string{{"Content-Type", "application/json"}},
		[]byte(`{"error": "`+message+`"}`),
		-1,
	)
}
