---
title: MCP Server 认证
keywords: [higress, mcp server auth]
description: MCP Server 认证插件配置参考（最小化 demo）
---

## 功能说明

`mcp-server-auth` 是一个最小化的鉴权 demo 插件，**从 HTTP 请求头解析 token**，并在路由/域名级用 `allow` 列表**直接校验合法 token**。没有 consumer/凭证间接层，便于作为入门示例阅读。

## 运行属性

插件执行阶段：`认证阶段`
插件执行优先级：`320`

## 配置字段

**注意：** token 的取值按头名区分（符合 RFC 6750）：

- 头名为 `Authorization`：**必须**携带 `Bearer ` scheme（大小写不敏感），即 `Authorization: Bearer <token>`；缺少 scheme 或非 Bearer 的值视为无效凭证，会触发 401。
- 其它自定义头（如 `x-api-key`、`ak`）：直接取**原始头值**作为 token，不带任何前缀。

### 认证配置（实例级）

| 名称   | 数据类型        | 填写要求 | 默认值 | 描述                         |
| ------ | --------------- | -------- | ------ | ---------------------------- |
| `keys` | array of string | 必填     | -      | token 的来源请求头名称列表   |

### 鉴权配置（路由/域名级）

| 名称    | 数据类型        | 填写要求                 | 默认值 | 描述                                                       |
| ------- | --------------- | ------------------------ | ------ | ---------------------------------------------------------- |
| `allow` | array of string | 必填（**非实例级别配置**） | -      | 只能在路由或域名等细粒度规则上配置，配置允许访问的合法 token |

> 行为：仅对配置了 `allow` 的路由/域名强制鉴权；未配置 `allow` 的路由（命中全局兜底配置）直接放行。

## 配置示例

在**实例级别**配置取值的请求头：

```yaml
keys:
- Authorization
```

对 route-a 这条路由配置允许的 token：

```yaml
allow:
- ak-123
```

此时只有携带合法 token 的请求被允许访问该路由：

**合法请求（放行）**
```bash
curl http://xxx.hello.com/test -H 'Authorization: Bearer ak-123'
```

**未提供 token，返回 401**
```bash
curl http://xxx.hello.com/test
```

**token 不在 allow 列表，返回 403**
```bash
curl http://xxx.hello.com/test -H 'Authorization: Bearer wrong-token'
```

> 在 MCP Inspector 里即：Custom Headers 增加一行 `Authorization` = `Bearer ak-123`。
> 也可以把 `keys` 配成 `x-api-key`、`allow` 配成纯 token，用 `x-api-key: ak-123` 形式访问。

## 相关错误码

| HTTP 状态码 | 出错信息                                                                            | 原因说明           |
| ----------- | ----------------------------------------------------------------------------------- | ------------------ |
| 401         | Request denied by MCP Server Auth check. No Key Authentication information found.    | 请求未提供 token   |
| 401         | Request denied by MCP Server Auth check. Multi Key Authentication information found. | 请求提供了多个 token |
| 403         | Request denied by MCP Server Auth check. Invalid token.                              | token 不在 allow 列表 |
