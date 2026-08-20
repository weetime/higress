# ai-credit-check

余额门禁插件。请求的 **username**（来自 `x-mse-consumer`）或 **API Key**（来自凭证头）命中欠费名单时返回 402，否则放行。

## 工作方式

| 项 | 值 |
|---|---|
| phase / priority | `AUTHZ` / `690`（在 ai-route-auth 700 之后，ai-quota-apikey 280 之前） |
| 身份来源（user 维度） | `x-mse-consumer`，取第一个 `/` 之前的部分作为 username，并裁掉前后空白 |
| 身份来源（apiKey 维度） | 请求凭证头中的**完整 apiKey**，解析顺序与 `ai-route-auth` 完全一致（见下） |
| 失效策略 | `FAIL_OPEN`（wasm 加载失败时全部放行且无任何告警，因此必须配一条 402 冒烟拨测作为兜底） |
| 外部依赖 | 无（不连 Redis、不发 HTTP、不读请求体） |

处理流程（两个维度是 **OR**，任一命中即拒绝）：

```
两个名单都为空                    → 放行（快速路径，连 header 都不读）
├─ blocked_users 非空
│    无 x-mse-consumer            → 跳过该维度
│    username 为空（畸形值）      → 跳过该维度
│    username 在名单中            → 402
└─ blocked_api_keys 非空
     取不到任何凭证头             → 跳过该维度
     apiKey 在名单中              → 402
其它                              → 放行
```

### apiKey 从哪来（为什么拿得到）

本插件是 `AUTHZ/690`，跑在 `ai-route-auth`(`AUTHZ/700`) **之后**、`ai-proxy` **之前**。
`ai-route-auth` 只【新增】`x-mse-consumer` / `x-api-key-name`，不会删改客户端带来的凭证头；
把 `Authorization` 换成上游 provider apiToken 的是更靠后的 `ai-proxy`。
所以本插件执行的这一刻，**客户端的原始凭证仍原样在请求头里**，可以拿到完整 apiKey。

（`x-mse-consumer` 里只有 apiKey 的**后 8 位**，不足以做完整比对，因此不用它。）

解析优先级与 `ai-route-auth/main.go` 的 `extractCredential` **严格对齐**——
两边不一致就会出现「`ai-route-auth` 按 key A 鉴权、本插件按 key B 查名单」的静默失效：

1. `X-HI-ORIGINAL-AUTH` —— 仅当 `x-higress-fallback-from` 存在（模型降级 `internal_redirect`
   重入）时才信任。此时 `Authorization` 已被上一趟 `ai-proxy` 换成上游 provider 的 apiToken，
   用户真实凭证在这个头里；首跳一律不信任它（该头不防伪造）。
2. 显式配置的 `auth_header_name`（非默认值时），按原值直取，不做 `Bearer` 解析。
3. `x-api-key` / `x-authorization` / `anthropic-api-key`（Anthropic / 透传风格）。
4. `Authorization: Bearer <key>`（OpenAI 风格；无 `Bearer` 前缀时按原值处理）。

> ⚠️ 只从**请求头**取 key。若某条链路把 key 放在 query 参数里（`ai-quota-apikey` 支持这种模式），
> 本插件的 apiKey 维度对它不生效——用 user 维度覆盖。

## 配置

```yaml
defaultConfig:
  blocked_users:
    alice: {}
    bob: {}
  blocked_api_keys:
    sk-user-aaaaaaaa: {}
  # auth_header_name: "Authorization"   # 缺省即 Authorization，一般无需配置
```

| 字段 | 含义 |
|---|---|
| `blocked_users` | key = username（`x-mse-consumer` 中第一个 `/` 之前的部分），`value` 无意义 |
| `blocked_api_keys` | key = **完整 apiKey**（与 `ai-route-auth` 的 `user_apikeys` 里的 key 同一份值），`value` 无意义 |
| `auth_header_name` | 从哪个头读 apiKey，缺省 `Authorization`；**必须与 `ai-route-auth` 配成同一个值** |

两个名单都是「只读 key」的 map，默认空 map 表示不拦截。

**`blocked_api_keys` 里只填 token 本身，不要带 `Bearer ` 前缀** —— 插件比对的是剥掉该前缀后的
裸 token，带前缀的名单项永远不会命中（这是最容易犯的误配置：直接从抓包/日志里整行复制
`Authorization` 头。插件会为此打 Warn，但不会去猜你的意图）。

`blocked_api_keys` 的意义是比 username 更细的封禁粒度：同一用户的多把 key 可以单独停用
（某把 key 泄露后被跑爆、或按 key 而非按人计费的场景）。

### 增删名单

用 JSON Merge Patch 单独增删一个 key，不需要 read-modify-write，并发更新不会互相覆盖：

```bash
# 余额 <= 0，拉黑用户
kubectl patch wasmplugin ai-credit-check.rise.io -n higress-system --type=merge \
  -p '{"spec":{"defaultConfig":{"blocked_users":{"alice":{}}}}}'

# 余额 > 0，解封（merge patch 中 null 表示删除该 key）
kubectl patch wasmplugin ai-credit-check.rise.io -n higress-system --type=merge \
  -p '{"spec":{"defaultConfig":{"blocked_users":{"alice":null}}}}'

# 停用 / 恢复单把 apiKey
kubectl patch wasmplugin ai-credit-check.rise.io -n higress-system --type=merge \
  -p '{"spec":{"defaultConfig":{"blocked_api_keys":{"sk-user-aaaaaaaa":{}}}}}'
kubectl patch wasmplugin ai-credit-check.rise.io -n higress-system --type=merge \
  -p '{"spec":{"defaultConfig":{"blocked_api_keys":{"sk-user-aaaaaaaa":null}}}}'
```

## 拒绝响应

```
HTTP/1.1 402 Payment Required
Content-Type: application/json

{"error":{"message":"Insufficient credit. Please top up your account.","type":"insufficient_quota","code":"insufficient_quota"}}
```

Envoy access log 的 `response_code_details` 会是 `ai-credit-check.insufficient_credit`。

user 维度与 apiKey 维度**共用同一个响应**（含 `response_code_details`）：对外不区分是「人欠费」
还是「这把 key 停用」，避免泄露内部计费口径。要区分时看网关日志里的 `Warn`，两者分别打了
`user "<username>"` 与 `api key ***<后 8 位>`（apiKey 只打后 8 位，不落完整凭证）。

## ⚠️ 覆盖面限制（接入前必读）

下面这一节只针对 **user 维度**（`blocked_users`）。**apiKey 维度不受这些限制**——
它直接读客户端自己带的凭证头，不依赖任何上游插件写入的身份，因此在**所有**路由上都生效，
也无法通过「路由没进 `ai-route-auth` 的 matchRules」绕开（客户端总不能不带自己的 key）。
需要一个不挑路由的兜底封禁时，用 `blocked_api_keys`。

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

**本插件是全局插件（`matchRules: []`）**，因此一旦某用户 / 某把 key 被拉黑，凡是携带其 consumer header
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
