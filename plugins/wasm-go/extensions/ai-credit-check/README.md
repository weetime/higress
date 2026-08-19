# ai-credit-check

余额门禁插件。请求携带 `x-mse-consumer` 时，若其用户名出现在欠费名单中则返回 402，否则放行。

## 工作方式

| 项 | 值 |
|---|---|
| phase / priority | `AUTHZ` / `690`（在 ai-route-auth 700 之后，ai-quota-apikey 280 之前） |
| 身份来源 | `x-mse-consumer`，取第一个 `/` 之前的部分作为 username，并裁掉前后空白 |
| 失效策略 | `FAIL_OPEN`（wasm 加载失败时全部放行且无任何告警，因此必须配一条 402 冒烟拨测作为兜底） |
| 外部依赖 | 无（不连 Redis、不发 HTTP、不读请求体） |

处理流程：

```
名单为空                → 放行
无 x-mse-consumer       → 放行
username 为空（畸形值） → 放行
username 在名单中       → 402
其它                    → 放行
```

## 配置

```yaml
defaultConfig:
  blocked_users:
    alice: {}
    bob: {}
```

`key` 是 username，`value` 无意义（约定写 `{}`，插件只读 key）。默认空 map 表示不拦截任何人。

### 增删名单

用 JSON Merge Patch 单独增删一个 key，不需要 read-modify-write，并发更新不会互相覆盖：

```bash
# 余额 <= 0，拉黑
kubectl patch wasmplugin ai-credit-check.rise.io -n higress-system --type=merge \
  -p '{"spec":{"defaultConfig":{"blocked_users":{"alice":{}}}}}'

# 余额 > 0，解封（merge patch 中 null 表示删除该 key）
kubectl patch wasmplugin ai-credit-check.rise.io -n higress-system --type=merge \
  -p '{"spec":{"defaultConfig":{"blocked_users":{"alice":null}}}}'
```

## 拒绝响应

```
HTTP/1.1 402 Payment Required
Content-Type: application/json

{"error":{"message":"Insufficient credit. Please top up your account.","type":"insufficient_quota","code":"insufficient_quota"}}
```

Envoy access log 的 `response_code_details` 会是 `ai-credit-check.insufficient_credit`。

## ⚠️ 覆盖面限制（接入前必读）

`x-mse-consumer` 有两个上游写入方，覆盖面不能只看其中一个：

- **`ai-route-auth`**（AUTHZ/700，本插件之前一步执行）。它有两条放行分支**不写**这个
  header（`main.go:310` 路由未命中任何 matchRule、`main.go:325` `user_apikeys` 未配置）；
  命中这两条分支时，客户端自带的 `x-mse-consumer` 会原样透传，可被用来绕过欠费拦截
  （不涉及越权访问，访问控制仍由 ai-route-auth 独立保证）。
- **`mcp-server-auth`**（`AUTHN/320`，属于 AUTHN 阶段，先于 AUTHZ 执行，因此也先于本插件）。
  它以相同的 `<username>/<apiKey 后 8 位>` 格式写 `x-mse-consumer`
  （`mcp-server-auth/main.go:316-320`）。关键区别是它的 `clearConsumerHeaders()`
  （`mcp-server-auth/main.go:330-334`）会在未通过鉴权的路径上主动 `RemoveHttpRequestHeader`
  掉客户端自带的 consumer 头——也就是说在 `mcp-server-auth` 命中的 MCP 路由上，
  本插件拿到的 `x-mse-consumer` **不可被客户端伪造**，比 `ai-route-auth` 那一半更可信。

> **本插件的实际覆盖面 = `ai-route-auth` matchRules 与 `mcp-server-auth` matchRules 的并集**，
> 两半的可信程度不同：`ai-route-auth` 那一半在上述两条分支上可被伪造，
> `mcp-server-auth` 那一半不可伪造。

**每新增一个需要计费的模型路由，必须同步把它的 ingress 加进 `ai-route-auth` 的 matchRules**，
否则该路由既不鉴权也不查钱包，且不会有任何报错。

**本插件是全局插件（`matchRules: []`）**，因此一旦某用户被拉黑，凡是携带其 consumer header
的路由——**包括 MCP 工具调用**——都会被拦截。MCP 场景下拦截返回的是 HTTP 402 与
OpenAI 风格的 JSON 错误体，而不是 JSON-RPC 错误，MCP 客户端可能无法按标准协议解析。
这是刻意的产品决策（没钱 = 停服，宁可牺牲一点协议一致性也要保证全局生效），
不是 bug，但接入方需要知道这一点。

完整分析见 `docs/superpowers/specs/2026-08-19-ai-credit-check-design.md` §6。

## 构建

必须在 `plugins/wasm-go/extensions/ai-credit-check/`（rsync 后的副本）下构建 ——
Makefile 的 `PARENT_DIR := ../..` 只有从那里才解析到有 Makefile 的 `plugins/wasm-go/`。

```bash
make test         # 单元测试（在本目录跑即可）
make local-build  # 本地编译 wasm，无需 docker
make build-push   # 构建并推送 OCI 镜像
```
