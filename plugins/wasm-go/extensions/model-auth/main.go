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

type ModelAuthConfig struct {
	apiKey string
}

func parseConfig(json gjson.Result, config *ModelAuthConfig, log log.Log) error {
	apiKey := json.Get("api_key").String()
	if apiKey == "" {
		return errors.New("api_key is required in configuration")
	}
	config.apiKey = apiKey
	return nil
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config ModelAuthConfig, log log.Log) types.Action {
	// Get the X-Model-Auth-Key header from the request
	authKey, err := proxywasm.GetHttpRequestHeader("X-Model-Auth-Key")
	if err != nil || authKey == "" {
		log.Warn("X-Model-Auth-Key header is missing")
		proxywasm.SendHttpResponseWithDetail(
			http.StatusUnauthorized,
			"model-auth.unauthorized",
			[][2]string{{"Content-Type", "text/plain"}},
			[]byte("Unauthorized: X-Model-Auth-Key header is required"),
			-1,
		)
		return types.ActionContinue
	}

	// Validate the API key
	if authKey != config.apiKey {
		log.Warnf("Invalid API key provided: %s", authKey)
		proxywasm.SendHttpResponseWithDetail(
			http.StatusForbidden,
			"model-auth.forbidden",
			[][2]string{{"Content-Type", "text/plain"}},
			[]byte("Forbidden: Invalid API key"),
			-1,
		)
		return types.ActionContinue
	}

	log.Info("Request authenticated successfully")
	return types.ActionContinue
}
