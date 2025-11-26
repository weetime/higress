# Model Auth Plugin

## 功能说明

`model-auth` 是一个 Higress WASM 插件，用于对 LLM 请求进行精细化的访问控制。插件验证请求中的 API Key 是否有权限访问特定的模型。

## 功能特性

- 验证 `Authorization` 头中的 Bearer Token（API Key）
- 验证 `x-higress-llm-model` 头中的模型名称
- 基于白名单控制每个 API Key 可以访问的模型
- 认证失败时返回 401 状态码及详细错误信息

## 配置说明

| 配置项 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| api_key_models | object | 是 | - | API Key 到允许访问的模型列表的映射 |
| auth_header_name | string | 否 | Authorization | 用于读取 API Key 的请求头名称 |
| model_header_name | string | 否 | x-higress-llm-model | 用于读取模型名称的请求头名称 |

### 配置格式

```yaml
api_key_models:
  <api-key>:
    - <model-name-1>
    - <model-name-2>
auth_header_name: "Authorization"  # 可选，默认为 "Authorization"
model_header_name: "x-higress-llm-model"  # 可选，默认为 "x-higress-llm-model"
```

## 配置示例

### 基本配置

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: model-auth
  namespace: higress-system
spec:
  defaultConfig:
    api_key_models:
      sk-test:
        - "gen-studio-Qwen2.5-0.5B-Instruct-bbbb"
      sk-weetime:
        - "gen-studio-Qwen2.5-0.5B-Instruct-bbbb"
        - "gen-studio-Qwen2.5-0.5B-Instruct-aaa"
      sk-audio:
        - "a1"
        - "a2"
  url: oci://<your_registry_hub>/model-auth:2.0.0
```

### 使用自定义请求头名称

如果你希望使用不同的请求头名称（例如 `X-API-Key` 和 `X-Model-Name`），可以这样配置：

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: model-auth
  namespace: higress-system
spec:
  defaultConfig:
    api_key_models:
      sk-test:
        - "model-1"
    auth_header_name: "X-API-Key"
    model_header_name: "X-Model-Name"
  url: oci://<your_registry_hub>/model-auth:2.0.0
```

### 按路由配置

```yaml
spec:
  defaultConfig:
    api_key_models:
      sk-default:
        - "model-1"
  matchRules:
  - config:
      api_key_models:
        sk-custom:
          - "model-2"
          - "model-3"
    ingress:
    - my-custom-route
```

## 使用示例

### 成功认证

```bash
curl -H "Authorization: Bearer sk-test" \
     -H "x-higress-llm-model: gen-studio-Qwen2.5-0.5B-Instruct-bbbb" \
     http://your-gateway/v1/chat/completions
```

### 常见错误响应

| 场景 | 返回信息 |
|------|----------|
| 缺少 Authorization 头 | `{"error": "Authorization header is required"}` |
| Authorization 格式错误 | `{"error": "Invalid Authorization header format. Expected: Authorization: Bearer <apiKey>"}` |
| 缺少模型头 | `{"error": "x-higress-llm-model header is required"}` |
| 无效的 API Key | `{"error": "Invalid API key"}` |
| 模型未授权 | `{"error": "Model access denied for this API key"}` |

## 构建说明

```bash
cd plugins/wasm-go && PLUGIN_NAME=model-auth IMG=quanzhenglong.com/camp/model-auth:v0.0.1  make build-push
```

## 安全建议

1. **保护 API Keys**
   - 不要将 API Key 硬编码在客户端代码中
   - 使用环境变量或密钥管理系统存储 API Keys
   - 使用 HTTPS 来保护传输中的 API Key

2. **定期轮换**
   - 定期轮换 API Keys
   - 为不同的用户或应用使用不同的 API Keys

3. **最小权限原则**
   - 只为 API Key 授予访问必要模型的权限
   - 定期审查和清理不再使用的 API Keys

4. **监控和日志**
   - 监控未授权的访问尝试
   - 启用审计日志记录所有认证事件

## 故障排除

| 问题 | 解决方案 |
|------|----------|
| 总是返回 401 | 检查 `Authorization` 头格式（必须是 `Bearer <token>`）<br>检查 API Key 是否在 `api_key_models` 配置中 |
| 某些模型无法访问 | 验证 `x-higress-llm-model` 头的值与配置中的模型名称完全匹配（区分大小写）<br>确认该模型在对应 API Key 的允许列表中 |
| 配置更新后不生效 | WasmPlugin 配置更新可能需要几秒钟才能生效<br>可通过 `kubectl get wasmplugin model-auth -n higress-system` 检查状态 |
