---
title: AI 配额管理（基于 API-Key）
keywords: [ AI网关, AI配额, API-Key ]
description: 基于 API-Key 的 AI 配额管理插件配置参考
---

## 功能说明

`ai-quota-apikey` 插件实现基于 API-Key 的 Token 配额管理，根据分配的固定配额进行配额策略限流，同时支持配额管理能力，包括查询单个配额、查询配额列表、刷新配额、增减配额。

`ai-quota-apikey` 插件需要配合 `ai-statistics` 插件获取 AI Token 统计信息。与 `ai-quota` 插件不同，本插件直接从请求中提取 API-Key，不需要配合认证插件（如 `key-auth`、`jwt-auth` 等）。

### 配额存储机制

插件使用两种Redis数据结构存储配额信息：

1. **单个配额Key**：`{redis_key_prefix}_{route_name}:{api_key}` -> 剩余配额（整数）
   - 用于快速查询单个API-Key的剩余配额
   - 每次配额消费时直接更新此key

2. **配额汇总Hash**：`{redis_key_prefix}_total_quota:{route_name}` -> `{api_key: "预设配额:剩余配额"}`
   - 用于批量查询某个route_name下所有API-Key的配额信息
   - Hash的field为api_key（如果开启了hash_api_key则存储hash值）
   - Hash的value格式为：`预设配额:剩余配额`（例如：`10000:5000`）
   - 每次配额更新时会同步更新Hash中的值

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

配额管理接口路径格式为 `/ai-quota-manager{admin_path}/{action}`，例如如果 `admin_path` 配置为 `/quota-manager`，则刷新 quota 可以通过：

```bash
curl https://example.com/ai-quota-manager/quota-manager/refresh \
  -H "Authorization: Bearer sk-admin-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "api_key": "sk-user-xxx",
    "quota": 10000,
    "route_name": "route1"
  }'
```

参数说明：
- `api_key`：要刷新的API-Key（必填）
- `quota`：新的配额值（必填，整数）
- `route_name`：路由名称（选填，如果不提供则使用配置中的route_name）

**注意**：所有管理接口的 POST 请求仅支持 JSON 格式，请求体必须是有效的 JSON。

刷新操作会：
1. 将剩余配额设置为指定的quota值
2. 将预设配额设置为指定的quota值（刷新时重置）
3. 同时更新单个配额Key和配额汇总Hash

Redis 中：
- 单个配额Key：`chat_quota_apikey_route1:{hash(sk-user-xxx)}` -> `10000`
- Hash中的值：`chat_quota_apikey_total_quota:route1` -> `{hash(sk-user-xxx): "10000:10000"}`

### 查询配额列表

查询指定route_name下所有API-Key的配额信息：

```bash
curl "https://example.com/ai-quota-manager/quota-manager/list?route_name=route1" \
  -H "Authorization: Bearer sk-admin-xxx"
```

参数说明：
- `route_name`：路由名称（选填，如果不提供则使用配置中的route_name）

将返回：
```json
[
  {
    "api_key": "hash_of_sk-user-xxx",
    "route_name": "route1",
    "preset_quota": 10000,
    "remaining_quota": 5000
  },
  {
    "api_key": "hash_of_sk-user-yyy",
    "route_name": "route1",
    "preset_quota": 20000,
    "remaining_quota": 15000
  }
]
```

**注意**：如果开启了`hash_api_key`，返回的`api_key`字段是hash后的值；如果未开启，则返回原始API-Key值。

### 增减 quota

增减特定 API-Key 的 quota 可以通过：

```bash
curl https://example.com/ai-quota-manager/quota-manager/delta \
  -H "Authorization: Bearer sk-admin-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "api_key": "sk-user-xxx",
    "value": 100,
    "route_name": "route1"
  }'
```

参数说明：
- `api_key`：要增减配额的API-Key（必填）
- `value`：增减的配额值（必填，整数，正数表示增加，负数表示减少）
- `route_name`：路由名称（选填，如果不提供则使用配置中的route_name）

操作说明：
- 如果`value`为正数，剩余配额会增加对应值
- 如果`value`为负数，剩余配额会减少对应值
- 预设配额保持不变
- 同时更新单个配额Key和配额汇总Hash

例如：如果当前剩余配额为5000，执行`value=100`后，剩余配额变为5100；执行`value=-200`后，剩余配额变为4800。

### 删除 quota

删除特定 API-Key 的配额可以通过：

```bash
curl https://example.com/ai-quota-manager/quota-manager/delete \
  -H "Authorization: Bearer sk-admin-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "api_key": "sk-user-xxx",
    "route_name": "route1"
  }'
```

参数说明：
- `api_key`：要删除配额的API-Key（必填）
- `route_name`：路由名称（选填，如果不提供则使用配置中的route_name）

操作说明：
- 删除操作会同时删除单个配额Key和Hash中的对应field
- 删除后，该API-Key的配额信息将完全清除
- **即使 key 不存在，删除操作也会返回 200 状态码**（幂等性保证）
- 如果Hash删除失败，会记录警告日志，但单个key已删除，操作仍返回成功

成功响应示例：
```json
{
  "message": "delete quota successful"
}
```

## 与 ai-quota 插件的区别

| 特性 | ai-quota | ai-quota-apikey |
|------|----------|-----------------|
| 配额标识 | Consumer 名称 | API-Key |
| 依赖认证插件 | 需要（key-auth/jwt-auth 等） | 不需要 |
| API-Key 提取 | 通过认证插件间接获取 | 直接从请求提取 |
| Redis Key | `{prefix}{consumer}` | `{prefix}_{route_name}:{api_key}` 或 `{prefix}_{route_name}:{hash(api_key)}` |
| 配额汇总存储 | 无 | `{prefix}_total_quota:{route_name}` (Hash) |
| 管理接口参数 | `consumer=xxx` | `api_key=xxx` |
| 管理认证 | `admin_consumer` | `admin_api_key` |
| 配额列表查询 | 不支持 | 支持（`/list`接口） |
| 预设配额记录 | 不支持 | 支持 |

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
   - **所有 POST 管理接口仅支持 JSON 格式**，不再支持 form-data 格式

4. **移除依赖**：
   - 可以移除 `key-auth` 等认证插件（如果不再需要）

## 管理接口汇总

管理接口路径格式：`/ai-quota-manager{admin_path}/{action}`

例如，如果 `admin_path` 配置为 `/quota-manager`，则完整路径为：
- `/ai-quota-manager/quota-manager/refresh` - 刷新配额
- `/ai-quota-manager/quota-manager/list` - 查询配额列表
- `/ai-quota-manager/quota-manager/delete` - 删除配额

| 接口路径 | 方法 | 功能 | 请求格式 | 参数 |
|---------|------|------|---------|------|
| `/ai-quota-manager{admin_path}/list` | GET | 查询指定route_name下所有API-Key的配额列表 | Query参数 | `route_name`（选填） |
| `/ai-quota-manager{admin_path}/refresh` | POST | 刷新配额（重置预设配额和剩余配额） | JSON | `api_key`（必填）、`quota`（必填）、`route_name`（选填） |
| `/ai-quota-manager{admin_path}/delta` | POST | 增减配额（只修改剩余配额） | JSON | `api_key`（必填）、`value`（必填）、`route_name`（选填） |
| `/ai-quota-manager{admin_path}/delete` | POST | 删除配额（删除单个配额Key和Hash中的field） | JSON | `api_key`（必填）、`route_name`（选填） |

**重要说明**：
- 所有管理接口都需要使用`admin_api_key`进行认证（通过Authorization header或query参数）
- **所有 POST 接口仅支持 JSON 格式**，请求体必须是有效的 JSON，Content-Type 应为 `application/json`
- 删除接口具有幂等性：即使 key 不存在，也会返回 200 状态码

## 注意事项

1. **API-Key 哈希**：强烈建议开启 `hash_api_key`，避免 Redis key 过长或包含特殊字符导致的问题
2. **API-Key 安全**：API-Key 会出现在请求头或查询参数中，建议使用 HTTPS 传输
3. **配额单位**：配额限制的是 Token 数量（输入 token + 输出 token），不是请求次数
4. **流式响应**：插件会在流式响应结束时根据实际使用的 token 数量扣减配额
5. **数据一致性**：插件会同时维护单个配额Key和配额汇总Hash，确保数据一致性。如果Hash更新失败，会记录警告日志但不影响主流程
6. **预设配额与剩余配额**：
   - **预设配额**：通过`refresh`接口设置的初始配额值，用于记录配额上限
   - **剩余配额**：当前可用的配额值，会随着token消费而减少，可通过`refresh`重置或通过`delta`增减
7. **配额列表查询性能**：使用Hash结构存储配额汇总信息，查询列表时只需一次Redis操作，性能优于逐个查询

