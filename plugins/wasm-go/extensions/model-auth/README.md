# Model Auth Plugin

## 功能说明

`model-auth` 是一个最小化的 WASM 认证插件示例，用于演示 Higress WASM 插件的基本流程。

## 功能特性

- 验证请求头中的 `X-Model-Auth-Key` 字段
- 支持配置预期的 API Key
- 认证失败时返回相应的 HTTP 状态码

## 配置说明

| 配置项 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| api_key | string | 是 | 预期的 API Key 值 |

## 配置示例

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: model-auth
  namespace: higress-system
spec:
  defaultConfig:
    api_key: "my-secret-key-123"
  url: oci://<your_registry_hub>/model-auth:1.0.0
```

## 使用示例

### 成功认证

```bash
curl -H "X-Model-Auth-Key: my-secret-key-123" http://your-gateway/api/test
```

### 认证失败（缺少密钥）

```bash
curl http://your-gateway/api/test
# 返回 401 Unauthorized: X-Model-Auth-Key header is required
```

### 认证失败（密钥错误）

```bash
curl -H "X-Model-Auth-Key: wrong-key" http://your-gateway/api/test
# 返回 403 Forbidden: Invalid API key
```

## 构建说明

```bash
PLUGIN_NAME=model-auth make build
```

## 技术说明

这是一个最小化实现的认证插件，主要用于：
1. 演示 Higress WASM 插件的基本结构
2. 验证 WASM 插件注册到 Higress 的完整流程
3. 作为开发其他插件的参考模板

