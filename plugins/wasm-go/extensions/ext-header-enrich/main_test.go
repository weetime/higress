package main

import (
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

const fullConfig = `{
  "auc": {
    "service_name": "auc.dns",
    "service_port": 8002,
    "path": "/v1/user/gateway-attributes",
    "gateway_token": "s3cr3t",
    "timeout": 2000
  },
  "redis": {
    "service_name": "redis-master.dns",
    "service_port": 6379,
    "timeout": 2000
  },
  "cache_ttl": 300,
  "negative_cache_ttl": 30,
  "cache_key_prefix": "ext_header_enrich",
  "on_error": "allow"
}`

func mustParse(t *testing.T, raw string) Settings {
	t.Helper()
	s, err := parseSettings(gjson.Parse(raw))
	require.NoError(t, err)
	return s
}

// ---------------------------------------------------------------- parseSettings

func TestParseSettingsFull(t *testing.T) {
	s := mustParse(t, fullConfig)

	require.Equal(t, "auc.dns", s.AUC.ServiceName)
	require.Equal(t, int64(8002), s.AUC.ServicePort)
	require.Equal(t, "/v1/user/gateway-attributes", s.AUC.Path)
	require.Equal(t, "s3cr3t", s.AUC.Token)
	require.Equal(t, uint32(2000), s.AUC.Timeout)
	require.Equal(t, "redis-master.dns", s.Redis.ServiceName)
	require.Equal(t, int64(6379), s.Redis.ServicePort)
	require.Equal(t, 300, s.CacheTTL)
	require.Equal(t, 30, s.NegativeCacheTTL)
	require.Equal(t, "ext_header_enrich", s.CacheKeyPrefix)
	require.False(t, s.DenyOnError)
}

// 只写必填项时，其余字段必须落到默认值上。
func TestParseSettingsAppliesDefaults(t *testing.T) {
	s := mustParse(t, `{
	  "auc":   {"service_name": "auc.dns", "service_port": 8002, "gateway_token": "t"},
	  "redis": {"service_name": "redis-master.dns", "service_port": 6379}
	}`)

	require.Equal(t, defaultAUCPath, s.AUC.Path)
	require.Equal(t, uint32(defaultAUCTimeout), s.AUC.Timeout)
	require.Equal(t, int64(defaultRedisTimeout), s.Redis.Timeout)
	require.Equal(t, defaultCacheTTL, s.CacheTTL)
	require.Equal(t, defaultNegativeCacheTTL, s.NegativeCacheTTL)
	require.Equal(t, defaultCacheKeyPrefix, s.CacheKeyPrefix)
	require.False(t, s.DenyOnError, "on_error 默认 allow")
}

// 必填项缺失必须报错。密钥尤其重要：AUC 侧是 fail-close，漏配的表现是
// 「插件没生效」而不是「配置报错」，不在这里拦住就极难排查。
func TestParseSettingsRequiredFields(t *testing.T) {
	cases := map[string]string{
		"auc.service_name":   `{"auc":{"service_port":8002,"gateway_token":"t"},"redis":{"service_name":"r","service_port":6379}}`,
		"auc.service_port":   `{"auc":{"service_name":"a","gateway_token":"t"},"redis":{"service_name":"r","service_port":6379}}`,
		"auc.gateway_token":  `{"auc":{"service_name":"a","service_port":8002},"redis":{"service_name":"r","service_port":6379}}`,
		"redis.service_name": `{"auc":{"service_name":"a","service_port":8002,"gateway_token":"t"},"redis":{"service_port":6379}}`,
		"redis.service_port": `{"auc":{"service_name":"a","service_port":8002,"gateway_token":"t"},"redis":{"service_name":"r"}}`,
		"empty config":       `{}`,
	}
	for name, raw := range cases {
		_, err := parseSettings(gjson.Parse(raw))
		require.Error(t, err, "missing %s should fail validation", name)
	}
}

func TestParseSettingsOnErrorDeny(t *testing.T) {
	s := mustParse(t, `{
	  "auc":   {"service_name":"a","service_port":8002,"gateway_token":"t"},
	  "redis": {"service_name":"r","service_port":6379},
	  "on_error": "DENY"
	}`)
	require.True(t, s.DenyOnError, "on_error 大小写不敏感")
}

func TestParseSettingsRejectsBadValues(t *testing.T) {
	base := `{"auc":{"service_name":"a","service_port":8002,"gateway_token":"t"},"redis":{"service_name":"r","service_port":6379}`
	for name, raw := range map[string]string{
		"on_error 非法值":          base + `,"on_error":"maybe"}`,
		"cache_ttl 为 0":         base + `,"cache_ttl":0}`,
		"cache_ttl 为负":          base + `,"cache_ttl":-1}`,
		"negative_cache_ttl 为负": base + `,"negative_cache_ttl":-1}`,
		"auc.timeout 为 0":       `{"auc":{"service_name":"a","service_port":8002,"gateway_token":"t","timeout":0},"redis":{"service_name":"r","service_port":6379}}`,
	} {
		_, err := parseSettings(gjson.Parse(raw))
		require.Error(t, err, name)
	}
}

// negative_cache_ttl: 0 是合法值（关闭 negative cache），但因为无法与「没配」区分，
// fillDefaults 会把它填回默认值 —— 这是已知取舍，锁住行为免得日后被当 bug 误改。
func TestNegativeCacheTTLZeroFallsBackToDefault(t *testing.T) {
	s := mustParse(t, `{
	  "auc":   {"service_name":"a","service_port":8002,"gateway_token":"t"},
	  "redis": {"service_name":"r","service_port":6379},
	  "negative_cache_ttl": 0
	}`)
	require.Equal(t, defaultNegativeCacheTTL, s.NegativeCacheTTL)
}

// ---------------------------------------------------------------- overrideSettings

// matchRule 是字段级覆盖而不是整体替换：只写 on_error 的规则必须继承密钥与连接信息。
func TestOverrideSettingsIsFieldLevel(t *testing.T) {
	base := mustParse(t, fullConfig)

	got, aucChanged, redisChanged, err := overrideSettings(gjson.Parse(`{"on_error":"deny"}`), base)

	require.NoError(t, err)
	require.True(t, got.DenyOnError)
	require.Equal(t, base.AUC, got.AUC, "未提及的 auc 配置必须原样继承")
	require.Equal(t, base.Redis, got.Redis)
	require.Equal(t, base.CacheTTL, got.CacheTTL)
	require.False(t, aucChanged, "只改 on_error 不该触发客户端重建")
	require.False(t, redisChanged)
}

// config: {} 表示完全继承。
func TestOverrideSettingsEmptyInheritsEverything(t *testing.T) {
	base := mustParse(t, fullConfig)

	got, aucChanged, redisChanged, err := overrideSettings(gjson.Parse(`{}`), base)

	require.NoError(t, err)
	require.Equal(t, base, got)
	require.False(t, aucChanged)
	require.False(t, redisChanged)
}

func TestOverrideSettingsDetectsConnectionChanges(t *testing.T) {
	base := mustParse(t, fullConfig)

	_, aucChanged, redisChanged, err := overrideSettings(gjson.Parse(`{"auc":{"service_port":9002}}`), base)
	require.NoError(t, err)
	require.True(t, aucChanged)
	require.False(t, redisChanged)

	_, aucChanged, redisChanged, err = overrideSettings(gjson.Parse(`{"redis":{"database":3}}`), base)
	require.NoError(t, err)
	require.False(t, aucChanged)
	require.True(t, redisChanged, "database 参与 Init，变了必须重建")
}

// path / token / timeout 是每次调用才用到的参数，不构成 AUC 客户端本身的变化。
func TestOverrideSettingsPathAndTokenDoNotRebuildClient(t *testing.T) {
	base := mustParse(t, fullConfig)

	got, aucChanged, _, err := overrideSettings(
		gjson.Parse(`{"auc":{"path":"/v2/attrs","gateway_token":"other","timeout":500}}`), base)

	require.NoError(t, err)
	require.Equal(t, "/v2/attrs", got.AUC.Path)
	require.Equal(t, "other", got.AUC.Token)
	require.Equal(t, uint32(500), got.AUC.Timeout)
	require.False(t, aucChanged)
}

// base 为零值（defaultConfig 缺失或被 defaultConfigDisable 丢掉）时，matchRule
// 自带完整配置也要能用。
func TestOverrideSettingsOnZeroBase(t *testing.T) {
	got, _, _, err := overrideSettings(gjson.Parse(fullConfig), Settings{})

	require.NoError(t, err)
	require.Equal(t, "auc.dns", got.AUC.ServiceName)
	require.Equal(t, defaultCacheTTL, got.CacheTTL)
}

// 覆盖后配置变得不合法（例如把密钥清空）必须报错，而不是静默发一个没带密钥的请求。
func TestOverrideSettingsValidatesResult(t *testing.T) {
	base := mustParse(t, fullConfig)

	_, _, _, err := overrideSettings(gjson.Parse(`{"auc":{"gateway_token":""}}`), base)
	require.Error(t, err)
}

// ---------------------------------------------------------------- resolveUsername

func TestResolveUsername(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		want   string
		ok     bool
	}{
		{"常规取值", map[string]string{consumerHeader: "zhangsan/1a2b3c4d"}, "zhangsan", true},
		{"无斜杠时整串即 username", map[string]string{consumerHeader: "zhangsan"}, "zhangsan", true},
		{"多个斜杠只取第一个之前", map[string]string{consumerHeader: "zhangsan/1a2b/3c4d"}, "zhangsan", true},
		{"两侧空白被裁掉", map[string]string{consumerHeader: "  zhangsan/1a2b3c4d  "}, "zhangsan", true},
		{"头缺失", map[string]string{}, "", false},
		{"头为空串", map[string]string{consumerHeader: ""}, "", false},
		{"头全是空白", map[string]string{consumerHeader: "   "}, "", false},
		{"以斜杠开头 username 为空", map[string]string{consumerHeader: "/1a2b3c4d"}, "", false},
		{"只有一个斜杠", map[string]string{consumerHeader: "/"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := resolveUsername(fakeHeaders(c.header))
			require.Equal(t, c.ok, ok)
			if c.ok {
				require.Equal(t, c.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------- buildUserHeaders

func TestBuildUserHeadersAllFields(t *testing.T) {
	got, err := buildUserHeaders([]byte(`{
	  "userId":"9f3c-4a2b","username":"zhangsan","name":"张三",
	  "email":"z@rise.io","mobile":"13800000000"
	}`))

	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"x-hgw-userid":   "9f3c-4a2b",
		"x-hgw-username": "zhangsan",
		"x-hgw-name":     "%E5%BC%A0%E4%B8%89",
		"x-hgw-email":    "z@rise.io",
		"x-hgw-mobile":   "13800000000",
	}, got)
}

// 中文姓名解码回来必须一字不差 —— 这是冒烟必测项在单测里的对应锁。
func TestBuildUserHeadersNameRoundTrips(t *testing.T) {
	got, err := buildUserHeaders([]byte(`{"name":"张三"}`))
	require.NoError(t, err)

	decoded, err := url.QueryUnescape(got["x-hgw-name"])
	require.NoError(t, err)
	require.Equal(t, "张三", decoded)
}

// 姓名里的空格必须编成 %20 而不是 '+'：下游 decodeURIComponent 不认 '+'，
// 会把它当字面加号留下（"张 三" -> "张+三"）。
func TestBuildUserHeadersNameSpaceUsesPercent20(t *testing.T) {
	got, err := buildUserHeaders([]byte(`{"name":"张 三"}`))
	require.NoError(t, err)
	require.NotContains(t, got["x-hgw-name"], "+")
	require.Contains(t, got["x-hgw-name"], "%20")

	decoded, err := url.QueryUnescape(got["x-hgw-name"])
	require.NoError(t, err)
	require.Equal(t, "张 三", decoded)
}

// 空串字段不注入空头 —— 下游拿到空 header 比拿不到更难判断。
func TestBuildUserHeadersSkipsEmptyFields(t *testing.T) {
	got, err := buildUserHeaders([]byte(`{"userId":"u1","username":"","name":"","email":"  ","mobile":"13800000000"}`))

	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"x-hgw-userid": "u1",
		"x-hgw-mobile": "13800000000",
	}, got)
}

// 一个字段都没有 / 解析不了 -> error。这条路径同时用于「缓存内容损坏 -> 回源」的判定。
func TestBuildUserHeadersErrors(t *testing.T) {
	for name, body := range map[string]string{
		"空 body":      ``,
		"非 json":      `not json at all`,
		"json 但不是对象":  `["a"]`,
		"对象但无属性字段":    `{"foo":"bar"}`,
		"属性全为空串":      `{"userId":"","name":""}`,
		"negative 标记": negativeMarker,
	} {
		_, err := buildUserHeaders([]byte(body))
		require.Error(t, err, name)
	}
}

// 明文字段里混进控制字符是 header 注入口子（email / username 是用户可影响的），
// 宁可少注入一个头也不写进去。
func TestBuildUserHeadersDropsControlCharacters(t *testing.T) {
	got, err := buildUserHeaders([]byte(`{"userId":"u1","email":"a@b.io\r\nx-hgw-username: admin"}`))

	require.NoError(t, err)
	require.Equal(t, map[string]string{"x-hgw-userid": "u1"}, got)
	require.NotContains(t, got, "x-hgw-email")
}

// ---------------------------------------------------------------- cacheKey / isManagedHeader

func TestCacheKey(t *testing.T) {
	require.Equal(t, "ext_header_enrich:zhangsan", cacheKey("ext_header_enrich", "zhangsan"))
	require.Equal(t, "ext_header_enrich:zhangsan", cacheKey("ext_header_enrich:", "zhangsan"),
		"前缀带不带尾冒号都得到同一个 key")
}

func TestIsManagedHeader(t *testing.T) {
	for _, name := range []string{"x-hgw-userid", "X-HGW-USERNAME", "X-Hgw-Name", "x-hgw-anything-else"} {
		require.True(t, isManagedHeader(name), name)
	}
	// x-mse-consumer 是【输入】，清掉它插件就没身份可用了。
	for _, name := range []string{consumerHeader, "authorization", "x-api-key-name", "x-hgwfoo", "hgw-x"} {
		require.False(t, isManagedHeader(name), name)
	}
}

// 注入的头名必须全部落在被清理的命名空间里，否则会留下清不掉的伪造头。
func TestEveryInjectedHeaderIsManaged(t *testing.T) {
	for _, a := range attributeHeaders {
		require.True(t, isManagedHeader(a.header), a.header)
	}
}
