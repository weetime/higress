---
title: AI 配额管理（基于 API-Key）
keywords: [ AI网关, AI配额, API-Key ]
description: 基于 API-Key 的 AI 配额管理插件配置参考
---

## 功能说明

`ai-quota-apikey` 插件实现基于 API-Key 的 Token 配额管理，根据分配的固定配额进行配额策略限流，同时支持配额管理能力，包括查询配额、刷新配额、增减配额。

`ai-quota-apikey` 插件需要配合 `ai-statistics` 插件获取 AI Token 统计信息。与 `ai-quota` 插件不同，本插件直接从请求中提取 API-Key，不需要配合认证插件（如 `key-auth`、`jwt-auth` 等）。

## 运行属性

插件执行阶段：`默认阶段`
插件执行优先级：`750`

## 配置说明

| 名称                 | 数据类型            | 填写要求                                 | 默认值 | 描述                                         |
|--------------------|-----------------|--------------------------------------| ---- |--------------------------------------------|
| `redis_key_prefix` | string          |  选填                                     |   `chat_quota_apikey:`   | quota redis key 前缀                         |
| `admin_api_key`   | string          | 必填                                   |      | 管理 quota 管理身份的 API-Key                 |
| `admin_path`       | string          | 选填                                   |   `/quota`   | 管理 quota 请求 path 前缀                        |
| `api_key_source`   | string          | 选填                                   |   `header`   | API-Key 来源：`header` 或 `query`                        |
| `api_key_header_name` | string          | 选填                                   |   `Authorization`   | 当 `api_key_source` 为 `header` 时，指定 header 名称                        |
| `api_key_query_name` | string          | 选填                                   |   `api_key`   | 当 `api_key_source` 为 `query` 时，指定查询参数名称                        |
| `hash_api_key`     | bool            | 选填                                   |   `false`   | 是否对 API-Key 进行 SHA256 哈希处理（推荐开启，避免 Redis key 过长或包含特殊字符）                        |
| `redis`            | object          | 必填                                    |      | redis相关配置                                  |

`redis`中每一项的配置字段说明

| 配置项       | 类型   | 必填 | 默认值                                                     | 说明                                                                                         |
| ------------ | ------ | ---- | ---------------------------------------------------------- | ---------------------------                                                                  |
| service_name | string | 必填 | -                                                          | redis 服务名称，带服务类型的完整 FQDN 名称，例如 my-redis.dns、redis.my-ns.svc.cluster.local |
| service_port | int    | 否   | 服务类型为固定地址（static service）默认值为80，其他为6379 | 输入redis服务的服务端口                                                                      |
| username     | string | 否   | -                                                          | redis用户名                                                                                  |
| password     | string | 否   | -                                                          | redis密码                                                                                    |
| timeout      | int    | 否   | 1000                                                       | redis连接超时时间，单位毫秒                                                                  |
| database     | int    | 否   | 0                                                          | 使用的数据库id，例如配置为1，对应`SELECT 1`                                                  |

## 配置示例

### 从 Authorization Header 提取 API-Key（推荐）

```yaml
redis_key_prefix: "chat_quota_apikey:"
admin_api_key: "sk-admin-xxx"
admin_path: /quota
api_key_source: "header"
api_key_header_name: "Authorization"
hash_api_key: true
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```

使用示例：
```bash
curl https://example.com/v1/chat/completions \
  -H "Authorization: Bearer sk-user-xxx" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "Hello"}]}'
```

### 从查询参数提取 API-Key

```yaml
redis_key_prefix: "chat_quota_apikey:"
admin_api_key: "sk-admin-xxx"
admin_path: /quota
api_key_source: "query"
api_key_query_name: "api_key"
hash_api_key: true
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```

使用示例：
```bash
curl "https://example.com/v1/chat/completions?api_key=sk-user-xxx" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "Hello"}]}'
```

### 刷新 quota

如果当前请求 url 的后缀符合 admin_path，例如插件在 example.com/v1/chat/completions 这个路由上生效，那么更新 quota 可以通过：

```bash
curl https://example.com/v1/chat/completions/quota/refresh \
  -H "Authorization: Bearer sk-admin-xxx" \
  -d "api_key=sk-user-xxx&quota=10000"
```

Redis 中 key 为 `chat_quota_apikey:{hash(sk-user-xxx)}` 的值就会被刷新为 10000（如果开启了 `hash_api_key`）。

### 查询 quota

查询特定 API-Key 的 quota 可以通过：

```bash
curl "https://example.com/v1/chat/completions/quota?api_key=sk-user-xxx" \
  -H "Authorization: Bearer sk-admin-xxx"
```

将返回：`{"api_key": "sk-user-xxx", "quota": 10000}`

### 增减 quota

增减特定 API-Key 的 quota 可以通过：

```bash
curl https://example.com/v1/chat/completions/quota/delta \
  -H "Authorization: Bearer sk-admin-xxx" \
  -d "api_key=sk-user-xxx&value=100"
```

这样 Redis 中 Key 为 `chat_quota_apikey:{hash(sk-user-xxx)}` 的值就会增加100，可以支持负数，则减去对应值。

## 与 ai-quota 插件的区别

| 特性 | ai-quota | ai-quota-apikey |
|------|----------|-----------------|
| 配额标识 | Consumer 名称 | API-Key |
| 依赖认证插件 | 需要（key-auth/jwt-auth 等） | 不需要 |
| API-Key 提取 | 通过认证插件间接获取 | 直接从请求提取 |
| Redis Key | `{prefix}{consumer}` | `{prefix}{api_key}` 或 `{prefix}{hash(api_key)}` |
| 管理接口参数 | `consumer=xxx` | `api_key=xxx` |
| 管理认证 | `admin_consumer` | `admin_api_key` |

## 迁移指南

从 `ai-quota` 迁移到 `ai-quota-apikey`：

1. **配置变更**：
   - 将 `admin_consumer` 改为 `admin_api_key`
   - 添加 `api_key_source`、`api_key_header_name`、`api_key_query_name`、`hash_api_key` 配置
   - 更新 `redis_key_prefix`（建议使用不同的前缀避免冲突）

2. **Redis 数据迁移**：
   - 需要将原有的 consumer 配额数据迁移到新的 API-Key 格式
   - 如果开启了 `hash_api_key`，需要计算 API-Key 的哈希值作为新的 key

3. **API 调用变更**：
   - 管理接口的参数从 `consumer=xxx` 改为 `api_key=xxx`
   - 请求中需要包含 API-Key（header 或 query 参数）

4. **移除依赖**：
   - 可以移除 `key-auth` 等认证插件（如果不再需要）

## 注意事项

1. **API-Key 哈希**：强烈建议开启 `hash_api_key`，避免 Redis key 过长或包含特殊字符导致的问题
2. **API-Key 安全**：API-Key 会出现在请求头或查询参数中，建议使用 HTTPS 传输
3. **配额单位**：配额限制的是 Token 数量（输入 token + 输出 token），不是请求次数
4. **流式响应**：插件会在流式响应结束时根据实际使用的 token 数量扣减配额

