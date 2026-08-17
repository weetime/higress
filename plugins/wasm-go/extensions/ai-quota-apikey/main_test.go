package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeHeaders 模拟 proxywasm.GetHttpRequestHeader：缺失的 header 返回 error，
// 与宿主行为一致（而不是返回空串）。
func fakeHeaders(kv map[string]string) headerGetter {
	return func(name string) (string, error) {
		if v, ok := kv[name]; ok {
			return v, nil
		}
		return "", errors.New("header not found")
	}
}

const (
	userKey     = "sk-1eb6d671-625c-4ffd-bed9-df3384d3a3b7"
	upstreamKey = "sk-infer-2e5e62e2-9cbd-4891-963d-250f141f4332"
)

// 降级重试：Authorization 已被上一趟的 ai-proxy 换成上游 provider 的 apiToken，
// 必须改用 ai-proxy 保存的原始凭证记账，否则用户配额不扣、账记到 apiToken 上。
func TestFallbackReentryUsesOriginalAuth(t *testing.T) {
	got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{
		"x-higress-fallback-from": "ai-route-infer-2e5e62e2.internal",
		"X-HI-ORIGINAL-AUTH":      "Bearer " + userKey,
		"Authorization":           "Bearer " + upstreamKey,
	}), defaultAuthHeaderName)

	require.NoError(t, err)
	require.Equal(t, userKey, got, "降级重试必须按用户原始 key 记账，而不是上游 apiToken")
}

// Anthropic 协议下降级重试：上游 token 落在 x-api-key 上，同样不能用它记账。
func TestFallbackReentryUsesOriginalAuthAnthropic(t *testing.T) {
	got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{
		"x-higress-fallback-from": "ai-route-infer-2e5e62e2.internal",
		"X-HI-ORIGINAL-AUTH":      "Bearer " + userKey,
		"x-api-key":               upstreamKey,
	}), defaultAuthHeaderName)

	require.NoError(t, err)
	require.Equal(t, userKey, got)
}

// 首跳（无 x-higress-fallback-from）必须忽略客户端伪造的 X-HI-ORIGINAL-AUTH。
// 否则调用方可以「用 A 通过鉴权、把账记到 B」。
func TestFirstHopIgnoresSpoofedOriginalAuth(t *testing.T) {
	got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{
		"X-HI-ORIGINAL-AUTH": "Bearer sk-someone-else",
		"Authorization":      "Bearer " + userKey,
	}), defaultAuthHeaderName)

	require.NoError(t, err)
	require.Equal(t, userKey, got, "首跳不得信任伪造的 X-HI-ORIGINAL-AUTH")
}

// 重入但缺 X-HI-ORIGINAL-AUTH 时回落到常规顺序，行为与首跳一致。
func TestFallbackReentryFallsBackToNormalOrder(t *testing.T) {
	got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{
		"x-higress-fallback-from": "ai-route-infer-2e5e62e2.internal",
		"Authorization":           "Bearer " + userKey,
	}), defaultAuthHeaderName)

	require.NoError(t, err)
	require.Equal(t, userKey, got)
}

// 回归锁：Anthropic 双协议提取（v0.0.19 加入，曾被 02f2952 误还原过一次）。
func TestAnthropicStyleHeaders(t *testing.T) {
	for _, h := range []string{"x-api-key", "x-authorization", "anthropic-api-key"} {
		got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{h: userKey}), defaultAuthHeaderName)
		require.NoError(t, err, h)
		require.Equal(t, userKey, got, h)
	}
}

// 回归锁：OpenAI 风格 Authorization: Bearer。
func TestOpenAIStyleBearer(t *testing.T) {
	got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{
		"Authorization": "Bearer " + userKey,
	}), defaultAuthHeaderName)

	require.NoError(t, err)
	require.Equal(t, userKey, got)
}

// 显式配置 api_key_header_name 时只认该 header，直取原值不做 Bearer 解析。
func TestConfiguredHeaderWins(t *testing.T) {
	got, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{
		"X-Route-API-Key": userKey,
		"Authorization":   "Bearer sk-should-be-ignored",
	}), "X-Route-API-Key")

	require.NoError(t, err)
	require.Equal(t, userKey, got)
}

// 一个凭证都没有时报错，由调用方转成 401。
func TestNoCredentialIsError(t *testing.T) {
	_, err := resolveApiKeyFromHeaders(fakeHeaders(map[string]string{}), defaultAuthHeaderName)
	require.Error(t, err)
}
