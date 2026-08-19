package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// parseConfig 必须永不失败。配合 FAIL_OPEN，解析报错只会让插件加载失败；
// 而计费系统写进 blocked_users 的形状可能千奇百怪，任何一种都不该拖垮插件。
func TestParseConfigNeverFails(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"空对象（字段缺失）", `{}`},
		{"空 map", `{"blocked_users":{}}`},
		{"单个用户", `{"blocked_users":{"alice":{}}}`},
		{"value 为 null", `{"blocked_users":{"alice":null}}`},
		{"value 为非空对象", `{"blocked_users":{"alice":{"reason":"debt"}}}`},
		{"空白 key 被忽略", `{"blocked_users":{"  ":{},"alice":{}}}`},
		{"类型错误：数组", `{"blocked_users":["alice"]}`}, // 端到端行为见 TestArrayBlockedUsersDoesNotSilentlyBlockWrongKey
		{"类型错误：字符串", `{"blocked_users":"alice"}`},
	}
	test.RunGoTest(t, func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				host, status := test.NewTestHost(json.RawMessage(c.cfg))
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)
			})
		}
	})
}

// ============================================================================
// 请求处理
// ============================================================================

const (
	cfgNoField       = `{}`
	cfgEmptyList     = `{"blocked_users":{}}`
	cfgAlice         = `{"blocked_users":{"alice":{}}}`
	cfgAliceAndBob   = `{"blocked_users":{"alice":{},"bob":{}}}`
	cfgAliceWithMeta = `{"blocked_users":{"alice":{"reason":"debt"}}}`
	cfgBlankAndAlice = `{"blocked_users":{"":{},"alice":{}}}`
)

// headersWith 构造一个典型的 chat completions 请求头，extra 用于追加 consumer。
func headersWith(extra ...[2]string) [][2]string {
	h := [][2]string{
		{":authority", "example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
	}
	return append(h, extra...)
}

// requireAllowed 断言请求被放行（未产生 local response）。
func requireAllowed(t *testing.T, cfg string, headers [][2]string, msg string) {
	t.Helper()
	host, status := test.NewTestHost(json.RawMessage(cfg))
	defer host.Reset()
	require.Equal(t, types.OnPluginStartStatusOK, status)

	require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders(headers))
	require.Nil(t, host.GetLocalResponse(), msg)
}

// requireDenied 断言请求被 402 拦截，且响应体是 OpenAI 兼容的 insufficient_quota。
func requireDenied(t *testing.T, cfg string, headers [][2]string, msg string) {
	t.Helper()
	host, status := test.NewTestHost(json.RawMessage(cfg))
	defer host.Reset()
	require.Equal(t, types.OnPluginStartStatusOK, status)

	host.CallOnHttpRequestHeaders(headers)
	resp := host.GetLocalResponse()
	require.NotNil(t, resp, msg)
	require.EqualValues(t, 402, resp.StatusCode)
	// 断言响应体字节级一致，防止 JSON 被改动或字段缺失
	expectedBody := []byte(`{"error":{"message":"Insufficient credit. Please top up your account.","type":"insufficient_quota","code":"insufficient_quota"}}`)
	require.Equal(t, expectedBody, resp.Data)
	// 断言 StatusCodeDetail 正确，Envoy 访问日志用此字段识别本插件拦截
	require.Equal(t, pluginName+".insufficient_credit", resp.StatusCodeDetail)
}

// 名单为空是常态（绝大多数时间没人欠费），必须全放行。
func TestEmptyBlockListAllowsEveryone(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, cfgEmptyList,
			headersWith([2]string{"x-mse-consumer", "alice/sk-aaa"}),
			"名单为空时不应拦截任何人")
	})
}

// blocked_users 字段整个缺失，等价于空名单。
func TestMissingFieldAllowsEveryone(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, cfgNoField,
			headersWith([2]string{"x-mse-consumer", "alice/sk-aaa"}),
			"blocked_users 缺失时不应拦截")
	})
}

// 没有 consumer header = 无身份，不是本插件管的事，放行。
func TestNoConsumerHeaderIsAllowed(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, cfgAlice, headersWith(),
			"没有 x-mse-consumer 时应放行")
	})
}

// consumer 为空串同样视为无身份。
func TestEmptyConsumerHeaderIsAllowed(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, cfgAlice,
			headersWith([2]string{"x-mse-consumer", ""}),
			"x-mse-consumer 为空串时应放行")
	})
}

// 核心用例：名单命中 -> 402。
func TestBlockedUserIsDenied(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireDenied(t, cfgAlice,
			headersWith([2]string{"x-mse-consumer", "alice/sk-aaa"}),
			"alice 在名单里，应被 402 拦截")
	})
}

// 名单未命中 -> 放行。
func TestUnblockedUserIsAllowed(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, cfgAlice,
			headersWith([2]string{"x-mse-consumer", "bob/sk-bbb"}),
			"bob 不在名单里，应放行")
	})
}

// consumer 不含 "/" 时整段当 username —— 兼容可能直接写纯用户名的上游。
func TestConsumerWithoutSlashIsMatched(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireDenied(t, cfgAlice,
			headersWith([2]string{"x-mse-consumer", "alice"}),
			"无斜杠的 consumer 应按整段匹配")
	})
}

// 名单里被误写入空 key 时，畸形 consumer 不能被连坐拦死。
func TestMalformedConsumerIsNotBlockedByBlankKey(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, cfgBlankAndAlice,
			headersWith([2]string{"x-mse-consumer", "/sk-xxx"}),
			"空 username 不应命中名单里的空 key")
	})
}

// header 值前后的空白必须被裁掉，否则会漏拦。
func TestConsumerIsTrimmed(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireDenied(t, cfgAlice,
			headersWith([2]string{"x-mse-consumer", "  alice/sk-aaa  "}),
			"前后空白不应导致漏拦")
	})
}

// 多用户名单。
func TestMultipleBlockedUsers(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireDenied(t, cfgAliceAndBob,
			headersWith([2]string{"x-mse-consumer", "bob/sk-bbb"}),
			"bob 也在名单里，应被拦截")
	})
}

// value 写了内容也只看 key —— 锁住「value 无意义」这个约定。
func TestBlockedUserValueIsIgnored(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireDenied(t, cfgAliceWithMeta,
			headersWith([2]string{"x-mse-consumer", "alice/sk-aaa"}),
			"value 非空不应影响命中")
	})
}

// blocked_users 被误写成 JSON 数组时，gjson 的 ForEach 会把数组下标（"0"）当成 key，
// 而不是数组元素本身，如果 parseConfig 不特判就会静默产生一个跟 alice 毫无关系的
// 「名单」——alice 实际没被拉黑。这里端到端断言：数组形状的 blocked_users 必须
// 等价于空名单，alice 应被放行。
func TestArrayBlockedUsersDoesNotSilentlyBlockWrongKey(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		requireAllowed(t, `{"blocked_users":["alice"]}`,
			headersWith([2]string{"x-mse-consumer", "0/sk-x"}),
			"blocked_users 是数组时应按空名单处理，不能误把数组下标当用户名拉黑")
	})
}
