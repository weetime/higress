---
title: MCP Server 认证
keywords: [higress, mcp server auth]
description: MCP Server 多维度授权插件配置参考
---

## 功能说明

`mcp-server-auth` 对 MCP Server 做**多维度授权**，三个维度按 **OR 语义**组合（命中任一即放行）：

1. **按用户分组**：用户归属若干 group，按 group 授权。
2. **按 api-key**：把某 apikey 直接授权到资源（独立放行通道，无需其 user 属于任何组）。
3. **按 tools 分组**：把工具归入工具集（tool group），按工具集授权，并**过滤 `tools/list` 响应**，
   让调用者只看到自己有权调用的工具。

授权模型为**主体 → 工具集绑定**：每条授权 = (主体: 某用户组 或 某 apikey) → 可调用的工具集列表。
可在同一个 MCP server 上给不同主体分配不同工具集。

插件基于 Higress **MCP Filter 框架**实现，会解析 JSON-RPC 请求体以拿到工具名，并在三个阶段生效：

| 阶段 | 时机 | 作用 |
| --- | --- | --- |
| 身份门禁 | 每个 JSON-RPC 请求 | 解析 token → user/group/apikey，判断是否允许访问本 server；命中即解析出「可见工具集」与「可调用工具集」，设置 `x-mse-consumer` |
| 工具调用鉴权 | `tools/call` | 工具名 → 所属工具集，不在**可调用工具集**内则返回 JSON-RPC error |
| 列表过滤 | `tools/list` 响应 | 仅保留调用者**可见**的工具 |

## 运行属性

插件执行阶段：`认证阶段`
插件执行优先级：`320`

## 配置字段

token 取值规则（符合 RFC 6750）：

- 头名为 `Authorization`：**必须**携带 `Bearer ` scheme（大小写不敏感）；缺少 scheme 或非 Bearer 视为无效凭证。
- 其它自定义头（如 `x-api-key`、`ak`）：直接取**原始头值**作为 token。

### 全局配置（实例级）—— 身份与工具字典

| 名称           | 数据类型           | 填写要求 | 描述                                       |
| -------------- | ------------------ | -------- | ------------------------------------------ |
| `keys`         | array of string    | 必填     | token 的来源请求头名称列表                 |
| `user_apikeys` | map: user → apikeys | 选填     | 用户持有的 apikey；内部反转为 apikey → user |
| `group_users`  | map: group → users | 选填     | 用户组成员；内部反转为 user → groups        |
| `tool_groups`  | map: 工具集 → tools | 选填     | 工具集到工具名列表的映射                   |

```yaml
keys:
- Authorization
user_apikeys:
  admin: ["sk-aaa", "ak-bbb"]
  alice: ["sk-ccc"]
group_users:
  ops:     ["admin"]
  readers: ["admin", "alice"]
tool_groups:
  read-tools:  ["get_weather", "search"]
  admin-tools: ["delete_resource", "reset"]
```

> **关于 `defaultConfigDisable: true`**：该开关为 `true` 时，Higress **不会**把 `defaultConfig`
> 传给插件（规则解析拿到的 globalConfig 为 nil），上面这些字典无法通过继承生效。此时请把
> `keys`/`user_apikeys`/`group_users`/`tool_groups` **直接写进每条 `matchRules[].config`**
> （与下方 `grants` 同级），插件会从规则自带的字典解析身份与工具映射。两种写法（写在
> `defaultConfig` 继承，或写在 `matchRules` 直供）都受支持。

### 路由/域名级配置 —— 授权（主体 → 工具集）

| 名称     | 数据类型        | 填写要求 | 描述                                          |
| -------- | --------------- | -------- | --------------------------------------------- |
| `grants` | array of object | 选填     | 授权列表，每项含主体（`group` 或 `apikey`）与 `tool_groups` |

其中每条 grant 的字段：

| 字段          | 数据类型        | 填写要求 | 描述                                          |
| ------------- | --------------- | -------- | --------------------------------------------- |
| `group`       | string          | 二选一   | 主体：用户组名（与 `apikey` 至少配一个）       |
| `apikey`      | string          | 二选一   | 主体：直授的 apikey                            |
| `tool_groups` | array of string | 必填     | 资源：授予的工具集名列表（`["*"]` 表示全部工具） |
| `list_only`   | boolean         | 选填     | 默认 `false`；`true` 时该 grant 的 `tool_groups` 只进入 `tools/list` 可见集，**不授予 `tools/call` 调用权** |

```yaml
grants:
  # 维度①：readers 组成员可调用 read-tools 工具集
  - group: readers
    tool_groups: ["read-tools"]
  # 维度①：ops 组可调用全部工具
  - group: ops
    tool_groups: ["*"]
  # 维度②：apikey sk-ccc 直授 read-tools（不要求其 user 属于任何组）
  - apikey: sk-ccc
    tool_groups: ["read-tools"]
  # 只读发现凭证：可查看全量 tools/list，但不能调用任何工具
  - apikey: sk-mcp-tools-discovery
    tool_groups: ["*"]
    list_only: true
```

规则说明：

- 每条 grant 必须含 `group` 或 `apikey` 其一（主体），加 `tool_groups`（资源）。
- `tool_groups: ["*"]` 表示该主体可调用本 server 全部工具，`tools/list` 不过滤。
- **OR 语义**：调用者命中任一 grant 的主体即「可访问该 server」。
- **可见集 vs 可调用集**：`tools/list` 可见集 = 所有命中 grant 的 `tool_groups` 并集；`tools/call` 可调用集 = 所有命中且**非 `list_only`** 的 grant 的 `tool_groups` 并集。缺省无 `list_only` 时两者相等，行为与旧配置一致。
- **只读发现语义**：`list_only: true` 配 `tool_groups: ["*"]` 即「可见全量工具、不可调用任何工具」；若某调用者**仅**命中 `list_only` 的 grant，则其可调用集为空，任何 `tools/call` 都会被拒。
- 路由级未配置任何 `grants`（命中全局兜底） → 直接放行，不强制鉴权（与未绑定路由不拦截一致）。
- **向后兼容**：旧 `allow: [token...]` 仍可用，等价于把这些 apikey 直授全部工具（`tool_groups: ["*"]`）。

## 行为示例

以上配置下，对某个 MCP server 路由：

- `alice`（`sk-ccc`，readers 组）调用 `get_weather` → 放行；调用 `delete_resource` → JSON-RPC error（无权）。
- `admin`（`sk-aaa`，ops 组）调用任意工具 → 放行。
- `alice` 请求 `tools/list` → 仅返回 `get_weather`、`search`。
- `sk-mcp-tools-discovery`（只读发现凭证）请求 `tools/list` → 返回**全部**工具；调用**任意**工具 → JSON-RPC error（无权）。
- 未携带 token → 401；携带多个 token → 401；未知 apikey 或不命中任何 grant → 403。

```bash
# 合法：readers 组成员调用授权工具
curl http://xxx.hello.com/mcp -H 'Authorization: Bearer sk-ccc' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{}}}'

# 只读发现凭证：可查看全量工具列表
curl http://xxx.hello.com/mcp -H 'Authorization: Bearer sk-mcp-tools-discovery' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
# 但不能调用任何工具 → 返回 JSON-RPC error (-32600)
curl http://xxx.hello.com/mcp -H 'Authorization: Bearer sk-mcp-tools-discovery' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_weather","arguments":{}}}'
```

## 已知限制

- **SSE 传输下的 `tools/list` 过滤**：列表过滤依赖读取 `application/json` 响应体。若 MCP 走 SSE
  （`text/event-stream`），`tools/list` 响应不会进入过滤器，列表过滤为 best-effort（无权工具可能仍出现在列表中）。
  此时真正的安全边界由 `tools/call` 工具调用鉴权保证——无权工具即使可见，调用也会被拒绝。

## 相关错误码

| HTTP 状态码 | 出错信息                                                                            | 原因说明                       |
| ----------- | ----------------------------------------------------------------------------------- | ------------------------------ |
| 401         | Request denied by MCP Server Auth check. No Key Authentication information found.    | 请求未提供 token               |
| 401         | Request denied by MCP Server Auth check. Multi Key Authentication information found. | 请求提供了多个 token           |
| 403         | Request denied by MCP Server Auth check. Credential is not allowed to access this MCP server. | 凭证不命中任何 grant（无权访问该 server） |
| JSON-RPC error (-32600) | tool "<name>" is not authorized                                         | 工具不在调用者被授予的工具集内 |
