# ext-header-enrich

在请求转发给上游之前，把调用方的用户属性以 `x-hgw-*` 请求头注入，供上游业务系统识别真实用户。

| | |
|---|---|
| phase / priority | `AUTHZ` / `600` |
| failStrategy | `FAIL_OPEN`（VM 故障时跳过本插件，不拖垮整个 listener；理由见 `wasmplugin.yaml` 注释） |
| 依赖 | AUC（`GET /v1/user/gateway-attributes`）、Redis |
| **强制前置插件** | **`ai-route-auth` 或 `mcp-server-auth`（见下）** |

## ⚠️ 强制前置依赖

**本插件必须与 `ai-route-auth` 或 `mcp-server-auth` 挂在同一条路由上，且排在其后执行。**

插件不自己解析 api-key，而是复用认证插件写入的 `x-mse-consumer`（取值 `<username>/<apikey后8位>`），
切出斜杠前半段作为 username。`x-mse-consumer` 在本插件眼里是**可信输入**，而它只是一个普通请求头 ——
客户端完全可以自带 `x-mse-consumer: admin/xxxxxxxx`。

挂了认证插件时这不构成风险：`ai-route-auth` 认证失败直接拒绝请求；`mcp-server-auth` 的
`clearConsumerHeaders()` 会在身份没解析出来时主动清掉客户端自带的这两个头。

但**单独挂载本插件，等于把用户邮箱、手机号做成匿名可读**。插件启动时会就此打一条 WARN 日志。

## 注入的头

五个属性各注入一个头，头名固定为 `x-hgw-<字段名小写>`，不可配。

| AUC 字段 | 头名 | 编码 |
|---|---|---|
| `userId` | `x-hgw-userid` | 明文（UUID，ASCII） |
| `username` | `x-hgw-username` | 明文（登录名，ASCII） |
| `email` | `x-hgw-email` | 明文（ASCII） |
| `mobile` | `x-hgw-mobile` | 明文（数字） |
| `name` | `x-hgw-name` | **percent-encode**（中文姓名） |

```
x-hgw-userid:   9f3c-4a2b-...
x-hgw-username: zhangsan
x-hgw-email:    z@rise.io
x-hgw-mobile:   13800000000
x-hgw-name:     %E5%BC%A0%E4%B8%89
```

- **AUC 未返回或返回空串的字段，对应的头不注入**（而不是注入一个空头）—— 下游拿到空 header 比拿不到更难判断。
- 只有 `name` 编码：其余四个字段都是 ASCII，直接进 header 没有问题；`name` 是中文姓名，
  而 HTTP header 值按 RFC 7230 是 ASCII 域，非 ASCII 字节属于已废弃的 `obs-text`，
  下游框架、日志系统、中间代理的处理各凭运气。
- 编码用 `url.PathEscape` 而非 `url.QueryEscape`：两者对中文结果相同，但空格前者编成 `%20`、
  后者编成 `+`，而下游最常用的 `decodeURIComponent` 不认 `+`，会把 `张 三` 变成 `张+三`。

下游读法 —— 四个明文头直接读，只有姓名要解一次：

```go
userID := r.Header.Get("x-hgw-userid")
name, _ := url.QueryUnescape(r.Header.Get("x-hgw-name"))
```

```js
const userId = req.headers['x-hgw-userid']
const name = decodeURIComponent(req.headers['x-hgw-name'] || '')
```

### 伪造防护

请求头阶段第一件事是**无条件清空整个 `x-hgw-` 命名空间** —— 遍历请求头，凡以 `x-hgw-` 开头的
一律移除，而不只是清将要注入的那五个。此步不可配、不可跳过、且在任何分支之前执行（包括
没命中 matchRule、配置非法、身份缺失这些分支）。不清的话，任何人自带一个 `x-hgw-username: admin`
就能冒充身份，而下游把这些头当可信输入。

按前缀清而非按配置清，是统一前缀带来的实质收益：配置里删掉一条映射、或不同路由注入的字段集
不同时，都不会留下漏网的伪造头。

`x-mse-consumer` 是**输入**，不在清理范围内 —— 清掉它插件就没身份可用了。

## 数据流

```
ai-route-auth / mcp-server-auth 认证通过后注入
        │
x-mse-consumer: <username>/<apikey后8位>
        │
        ├─ 取斜杠前半段 ──► username
        │
        ├─ Redis 命中？ ──是──► 属性 JSON
        │        └─否──► AUC GET /v1/user/gateway-attributes?username=… ──► 属性 JSON ──► 回写 Redis
        │
        └─► 注入 x-hgw-* 明文头 ──► 上游
```

- 缓存 key 用 **username 而非 api-key**：同一个人的多把 key 共享一份缓存，属性本来就按人变化。
- 缓存里存的是 **AUC 的原始 JSON**（不是拆好的头）：Redis 里的值肉眼可读、便于排查，
  且「缓存内容损坏 → 回源」这条路径能靠 JSON 解析判定。
- 回源期间请求头阶段返回 `HeaderStopAllIterationAndWatermark`(4) 挂起。**不能用
  `HeaderStopIteration`(1)**：后者只停 header filter 的迭代，请求仍会继续走下去，
  等回调回来时头已经发给上游了，注入不生效。

### negative cache

AUC 返回 404 时往缓存里写一个短 TTL 的标记（`__ext_header_enrich_absent__`），命中它直接走
`on_error`，不回源。

- **为什么需要**：一把仍在流通、但对应用户已被删除或离职销号的 api-key，不缓存失败结果的话，
  它的**每个请求**都会同步回源一次 AUC。离职销号是常态，不是边缘情况。
- **为什么只缓存 404**：404 是「这个用户确实不存在」，是稳定的事实；5xx / 超时是后端故障，
  缓存它会让 AUC 恢复之后网关还要多扛一个 TTL 才恢复正常。

## 配置

```yaml
auc:
  service_name: auc.dns              # 必填。网关侧需能解析
  service_port: 80                   # 必填。填 Service 端口，不是容器端口（AUC 容器听 8002，Service 暴露 80）
  gateway_token: "<共享密钥>"        # 必填。以 x-gateway-token 头发出
  path: /v1/user/gateway-attributes  # 可选
  timeout: 2000                      # 可选，毫秒
redis:
  service_name: redis-master.dns     # 必填
  service_port: 6379                 # 必填
  timeout: 2000                      # 可选，毫秒
  # username / password / database 可选，与 ai-quota-apikey 同构
cache_ttl: 300                       # 可选，秒
negative_cache_ttl: 30               # 可选，秒
cache_key_prefix: ext_header_enrich  # 可选
on_error: allow                      # 可选，allow | deny
```

**注入什么头不用配** —— 字段就固定那五个，头名按 `x-hgw-<字段名小写>` 机械生成。

**matchRule 级配置是字段级覆盖**，不是整体替换：`defaultConfig` 那份是基底，matchRule 的
`config` 只写要改的字段即可（`config: {}` 表示完全继承）。某条路由想单独设 `on_error: deny`
时只写这一个字段，不必重复抄一遍密钥与连接信息。

> **`defaultConfigDisable` 必须是 `false`。** higress 的 `convertIstioWasmPlugin`
> 在它为 `true` 时会把整个 `defaultConfig` 丢掉、只下发 `_rules_`
> （`higress/pkg/ingress/config/ingress_config.go`），上面的「基底 + 字段级覆盖」模型就不成立了。
> 代价是没命中任何 matchRule 的路由也会执行本插件 —— 这种情况下插件只做 `x-hgw-*` 清理，
> 不做富化、不打 Redis、也不回源 AUC。

> **要关闭 negative cache，请在 `defaultConfig` 里显式写 `negative_cache_ttl: 0`。**
> 写在 matchRule 里的 `0` 无法与「没配这一项」区分，会被填回默认值 30。

## 错误语义

统一收敛到 `on_error`：

| 情形 | 处理 |
|---|---|
| `x-mse-consumer` 缺失、为空、或切出的 username 为空 | on_error |
| AUC 返回 404（用户不存在/已禁用） | on_error + **写 negative 缓存** |
| AUC 超时、连接失败、5xx | on_error，**不写缓存** |
| AUC 响应 JSON 解析失败，或五个属性字段一个都没有 | on_error，**不写缓存** |
| 配置非法（缺密钥、缺服务名等） | on_error，并打 ERROR 日志 |
| Redis 不可用 / 未命中 | **不是错误**，直接回源 AUC |
| Redis 命中 negative 标记 | on_error，不回源 |
| 未命中任何 matchRule | 不是错误，只清理 `x-hgw-*`，不富化 |

- `on_error: allow`（默认）：放行，但**不注入任何 `x-hgw-*` 头**。清理步骤已经执行，
  下游看到的是「无身份」，而不是「伪造身份」。
- `on_error: deny`：返回 503，响应体不区分「用户不存在」与「AUC 挂了」（区分开会变成一个账号探测口子）。

默认选 `allow` 的理由：本插件职责是信息富化不是认证，认证由 `ai-route-auth` / `mcp-server-auth`
把关；AUC 抖动不应把业务流量打挂。

**配置解析永不返回 error**：wasm-go 下任意一段配置解析失败都会让整个插件配置加载失败，
会把网关上**所有**路由一起打挂（注意这与 `failStrategy` 无关——那一项管的是 VM 故障，
这里说的是配置解析失败导致整份配置被拒）。因此非法配置只会让该路由走
`on_error`，并在启动日志里打 ERROR。

## 已知限制

- **TTL 内 AUC 改动不生效。** `cache_ttl` 默认 300s，用户改了邮箱最长 5 分钟后才反映到网关。
  暂不做主动失效。
- **缓存击穿未处理。** 同一个 username 的并发请求会同时未命中、同时回源；冷启动或 TTL 到期
  的瞬间 AUC 会收到 N 倍于稳态的请求。wasm 插件做 singleflight 代价很高（每个 worker VM
  各自独立、无共享状态）。若 AUC 侧观察到明显的周期性尖峰，优先考虑给 `cache_ttl` 加随机抖动。
- **共享密钥明文落在 WasmPlugin CR 里**，与现有 api-key 的处置方式一致。密钥是最小权限的 ——
  只能换到五个白名单属性字段，不复用 `ADMIN_JWT_TOKEN`。
- **仍有一层只能靠冒烟。** 宿主级测试（见下）已经覆盖清头、注入、Redis / AUC 调用、挂起恢复
  这些逻辑，但它模拟的是 envoy 的 ABI，模拟不了真实环境：服务发现能不能解析 `auc.dns`、
  插件在 filter chain 里的实际执行顺序、`x-mse-consumer` 是否真被上游认证插件写上。
  这几项只能在集群里验。

## 开发

```bash
make test          # 单测
make build         # 构建 wasm
make build-push    # 构建并推镜像
```

测试分两层。

**第一层 `main_test.go`** —— 把纯逻辑抽成不碰 proxywasm 的函数再用 testify 测
（范式同 `ai-quota-apikey/main_test.go`）：

| 纯函数 | 职责 |
|---|---|
| `parseSettings` | 必填校验与默认值，用于 `defaultConfig` |
| `overrideSettings` | matchRule 的字段级覆盖 + 「连接参数是否变化」判定 |
| `resolveUsername` | 读 `x-mse-consumer` 并切出斜杠前半段 |
| `buildUserHeaders` | 解析属性 JSON 生成待注入的头集合 |
| `cacheKey` | 缓存 key 拼接 |
| `isManagedHeader` | 判定是否属于 `x-hgw-` 命名空间 |

**第二层 `host_test.go`** —— 用 wasm-go 自带的 proxy-wasm 宿主模拟器
（`github.com/higress-group/wasm-go/pkg/test`）跑完整的请求头链路，覆盖上面那些纯函数
测不到的薄壳：清头、注入、挂起/恢复、Redis 调用、AUC 回源、`on_error` 分支。
框架能直接喂 Redis 与 AUC 的响应（`CallOnRedisCall` / `CallOnHttpCall`），
并读回最终发给上游的请求头（`GetRequestHeaders`）与本地响应（`GetLocalResponse`）。

```bash
go test -run TestHost ./...        # Go 模式，秒级

# 想连编译产物一起验（wasm 模式默认因找不到文件而 SKIP）：
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm ./
go test -run TestHost ./...        # 每个用例多跑一遍 wasm 模式，约 5s/例
```

`main.wasm` 已在仓库 `.gitignore` 里，不要提交。

### 冒烟必测项

宿主测试覆盖不到环境相关的部分，下面这些仍需在集群里验：

1. **Redis 未命中** —— 首个请求回源 AUC，五个头内容正确，`x-hgw-name` 解码后中文不乱码；
   Redis 里出现 `ext_header_enrich:<username>`，值是 AUC 的原始 JSON。
2. **Redis 命中** —— 第二个请求不再打 AUC（AUC 侧无新增访问日志），头依然正确。
3. **AUC 不可用** —— 停掉 AUC 或改错 `service_name`：`on_error: allow` 下请求放行且**不带**
   任何 `x-hgw-*` 头；改成 `on_error: deny` 后返回 503。
4. **客户端伪造 `x-hgw-*` 被清理** —— 请求自带 `x-hgw-username: admin`，上游收到的是 AUC 的
   真实值（或在 on_error 分支下收不到该头），绝不能是 `admin`。
5. **边界确认（预期行为，不是 bug）** —— 在一条**不挂**认证插件的路由上单独挂本插件，
   伪造 `x-mse-consumer: <别人>/xxxxxxxx` 能拿到那个人的属性。测它是为了确认边界被理解。

> 冒烟前必须先确认 `auc.service_name` 在网关侧可解析（集群侧要有服务发现条目）。
> 否则所有回源都会失败并在 `on_error: allow` 下静默放行，表现为「插件没生效」而不是报错。
