# Model Auth Plugin

## 功能说明

`model-auth` 是一个 Higress WASM 插件，基于**租户和工作空间**的多级权限模型，为 LLM 模型请求提供精细化的访问控制。

## 功能特性

- ✅ 基于租户（Tenant）和用户（username）的多级权限控制
- ✅ 支持模型级别的访问控制（哪些租户可以访问哪些模型）
- ✅ 支持租户级别的用户管理（每个租户包含哪些用户）
- ✅ 支持通配符 `"*"` 允许所有用户访问特定模型
- ✅ 灵活的配置：只对配置的模型进行鉴权，未配置的模型直接放行
- ✅ 自动设置 `x-api-key-name` header 标识用户身份
- ✅ 认证失败返回详细的错误信息（401/403）

---

## 核心概念

### 三层映射关系

插件使用三层映射来实现灵活的权限控制：

```
┌─────────────────────────────────────────────────────────────────┐
│                       鉴权流程架构                                │
└─────────────────────────────────────────────────────────────────┘

   Request Headers
   ├─ Authorization: Bearer sk-test
   └─ x-higress-llm-model: model-a
          │
          ▼
   ┌──────────────────────┐
   │  1. model_mapping    │  模型 → 允许访问的租户列表
   └──────────────────────┘
          │
          ├─ "*" → 所有用户都可以访问
          └─ ["tenant-1", "tenant-2"] → 只有特定租户的用户可访问
          │
          ▼
   ┌──────────────────────┐
   │ 2. api_key_mapping   │  用户 → API Key 列表
   └──────────────────────┘
          │
          └─ user-1: [sk-key-1, sk-key-2]
             (内部反向映射为 sk-key-1 → user-1)
          │
          ▼
   ┌──────────────────────┐
   │ 3. workspace_mapping │  租户 → 用户列表
   └──────────────────────┘
          │
          └─ tenant-1: [user-1, user-2]
          │
          ▼
   ┌──────────────────────┐
   │   权限验证通过 ✅     │
   └──────────────────────┘
```

### 1. model_mapping（模型映射）

定义每个模型允许哪些**租户**访问：

```yaml
model_mapping:
  model-public:
    - "*"                    # 所有用户都可以访问
  model-enterprise:
    - "tenant-1"             # 只有 tenant-1 和 tenant-2 的用户可以访问
    - "tenant-2"
```

### 2. api_key_mapping（API Key 映射）

定义每个用户拥有的 **API Key 列表**（一个用户可以有多个 API Key）：

```yaml
api_key_mapping:
  user-1:                   # 用户 user-1 拥有以下 API Keys
    - "sk-02a69aa1-df0e-4ecb-8e02-a0b832d56295"
    - "sk-1d25a264-fad1-46de-95e7-54621d827d7e"
  user-2:                   # 用户 user-2 拥有以下 API Key
    - "sk-e377ddd2-88f2-47cb-9e2c-fe174dca1bd0"
```

### 3. workspace_mapping（工作空间映射）

定义每个租户包含哪些**用户**：

```yaml
workspace_mapping:
  tenant-1:
    - "user-1"              # tenant-1 包含 user-1 和 user-2
    - "user-2"
  tenant-2:
    - "user-3"              # tenant-2 包含 user-3
```

---

## 完整鉴权流程

```
┌─────────────────────────────────────────────────────────────────┐
│                       详细鉴权流程                                │
└─────────────────────────────────────────────────────────────────┘

Step 1: 获取模型名称
├─ 从 request header 读取 model_header_name（默认 x-higress-llm-model）
└─ 如果没有 model header → 🟢 跳过鉴权，直接放行

Step 2: 检查模型是否归本插件管理
├─ 检查 model 是否在 model_mapping 中
└─ 如果不在 → 🟢 跳过鉴权，直接放行（该模型不归本插件管理）

Step 3: 获取并验证 API Key
├─ 从 request header 读取 auth_header_name（默认 Authorization）
├─ 如果缺少 auth header → 🔴 返回 401 "Authorization header is required"
├─ 提取 API Key（如果是 Authorization header，需要 Bearer Token 格式）
└─ 如果格式错误 → 🔴 返回 401 "Invalid Authorization header format"

Step 4: 获取用户并设置身份标识
├─ 从 api_key_mapping[apiKey] 获取 userName
├─ 如果 API Key 不存在 → 🔴 返回 401 "Invalid API key"
├─ 设置 x-api-key-name header = userName + apiKey后8位
└─ 记录用户身份用于后续验证

Step 5: 检查通配符权限
├─ 从 model_mapping[model] 获取 allowedTenants
├─ 如果 allowedTenants = ["*"]
│   ├─ 🟢 通配符模式，所有用户都可以访问
│   └─ ✅ 鉴权通过，放行请求
└─ 否则 → 继续 Step 6

Step 6: 验证用户租户权限
├─ 遍历 allowedTenants 列表
├─ 对每个 tenant，从 workspace_mapping[tenant] 获取 users
├─ 检查 userName 是否在 users 列表中
│   ├─ 如果找到 → ✅ 鉴权通过，放行请求
│   └─ 如果未找到 → 继续检查下一个 tenant
└─ 如果所有 tenant 都不包含该用户 → 🔴 返回 403 "Access denied"

┌─────────────────────────────────────────────────────────────────┐
│                       返回结果                                    │
└─────────────────────────────────────────────────────────────────┘

✅ 鉴权成功：
   - 请求继续转发到后端服务
   - x-api-key-name header 已设置为 userName + apiKey后8位

🔴 鉴权失败：
   - 401: 缺少认证信息、格式错误、无效的 API Key
   - 403: 用户没有权限访问该模型
```

---

## 配置说明

### 配置项

| 配置项 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| model_mapping | object | 是 | - | 模型到允许访问的租户列表的映射 |
| workspace_mapping | object | 是 | - | 租户到用户列表的映射 |
| api_key_mapping | object | 是 | - | 用户名到 API Key 列表的映射（一个用户可以有多个 API Key） |
| auth_header_name | string | 否 | Authorization | 用于读取 API Key 的请求头名称 |
| model_header_name | string | 否 | x-higress-llm-model | 用于读取模型名称的请求头名称 |

### 配置格式

```yaml
model_mapping:
  <model-name>:
    - "*"                    # 或者特定租户列表
    - "<tenant-name>"

workspace_mapping:
  <tenant-name>:
    - "<user-name-1>"
    - "<user-name-2>"

api_key_mapping:
  <user-name>:              # 用户名作为 key
    - "<api-key-1>"         # 该用户的 API Key 列表
    - "<api-key-2>"

auth_header_name: "Authorization"      # 可选
model_header_name: "x-higress-llm-model" # 可选
```

---

## 配置示例

### 示例 1：基本配置

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: model-auth
  namespace: higress-system
spec:
  defaultConfig:
    # 模型映射：定义每个模型允许哪些租户访问
    model_mapping:
      qwen-turbo:
        - "*"                              # 所有用户都可以访问
      qwen-plus:
        - "enterprise-tenant"              # 只有企业租户可以访问
        - "premium-tenant"
      qwen-max:
        - "enterprise-tenant"              # 只有企业租户可以访问

    # 工作空间映射：定义每个租户包含哪些用户
    workspace_mapping:
      enterprise-tenant:
        - "alice"
        - "bob"
      premium-tenant:
        - "charlie"

    # API Key 映射：定义每个用户拥有的 API Key 列表
    api_key_mapping:
      alice:
        - "sk-alice-key-1"
        - "sk-alice-key-2"
      bob:
        - "sk-bob-key-1"
      charlie:
        - "sk-charlie-key-1"

  url: oci://quanzhenglong.com/camp/model-auth:v0.0.10
```

### 示例 2：多租户场景

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: model-auth
  namespace: higress-system
spec:
  defaultConfig:
    model_mapping:
      # 公共模型
      gpt-3.5-turbo:
        - "*"
      
      # 部门专用模型
      finance-model:
        - "finance-dept"
      hr-model:
        - "hr-dept"
      
      # 跨部门共享模型
      analytics-model:
        - "finance-dept"
        - "marketing-dept"

    workspace_mapping:
      finance-dept:
        - "finance-user-1"
        - "finance-user-2"
        - "finance-admin"
      hr-dept:
        - "hr-user-1"
        - "hr-manager"
      marketing-dept:
        - "marketing-user-1"
        - "marketing-lead"

    api_key_mapping:
      finance-user-1:
        - "sk-finance-1-key-1"
        - "sk-finance-1-key-2"
      finance-user-2:
        - "sk-finance-2-key-1"
      finance-admin:
        - "sk-finance-admin-key-1"
      hr-user-1:
        - "sk-hr-1-key-1"
      hr-manager:
        - "sk-hr-mgr-key-1"
      marketing-user-1:
        - "sk-mkt-1-key-1"
      marketing-lead:
        - "sk-mkt-lead-key-1"

  url: oci://quanzhenglong.com/camp/model-auth:v0.0.10
```

### 示例 3：使用自定义请求头

```yaml
spec:
  defaultConfig:
    model_mapping:
      custom-model:
        - "custom-tenant"

    workspace_mapping:
      custom-tenant:
        - "user-a"

    api_key_mapping:
      user-a:
        - "sk-custom-key-1"
        - "sk-custom-key-2"

    # 自定义请求头名称
    auth_header_name: "X-API-Key"        # 使用自定义 API Key header
    model_header_name: "X-Model-Name"    # 使用自定义模型 header

  url: oci://quanzhenglong.com/camp/model-auth:v0.0.10
```

### 示例 4：按路由配置

```yaml
spec:
  # 默认配置
  defaultConfig:
    model_mapping:
      default-model:
        - "default-tenant"
    workspace_mapping:
      default-tenant:
        - "default-user"
    api_key_mapping:
      default-user:
        - "sk-default-key-1"

  # 路由级别配置（覆盖默认配置）
  matchRules:
  - config:
      model_mapping:
        route-specific-model:
          - "special-tenant"
      workspace_mapping:
        special-tenant:
          - "special-user"
      api_key_mapping:
        special-user:
          - "sk-special-key-1"
    ingress:
    - my-custom-route
```

---

## 使用示例

### 场景 1：访问公共模型（通配符）

```bash
# 使用 alice 的 API Key 访问公共模型（允许所有用户）
curl -H "Authorization: Bearer sk-alice-key-1" \
     -H "x-higress-llm-model: qwen-turbo" \
     http://your-gateway/v1/chat/completions

# ✅ 成功：qwen-turbo 配置为 "*"，所有用户都可以访问
# x-api-key-name: alice/key-1 (alice + "/" + API Key后8位)
```

### 场景 2：访问租户专属模型

```bash
# alice (enterprise-tenant) 访问企业模型，可以使用任何一个已配置的 API Key
curl -H "Authorization: Bearer sk-alice-key-1" \
     -H "x-higress-llm-model: qwen-plus" \
     http://your-gateway/v1/chat/completions

# ✅ 成功：alice 属于 enterprise-tenant，有权访问 qwen-plus
```

### 场景 3：同一用户使用不同 API Key

```bash
# alice 使用第二个 API Key 访问
curl -H "Authorization: Bearer sk-alice-key-2" \
     -H "x-higress-llm-model: qwen-plus" \
     http://your-gateway/v1/chat/completions

# ✅ 成功：sk-alice-key-2 也属于 alice，权限相同
```

### 场景 4：跨租户访问被拒绝

```bash
# charlie (premium-tenant) 尝试访问企业专属模型
curl -H "Authorization: Bearer sk-charlie-key-1" \
     -H "x-higress-llm-model: qwen-max" \
     http://your-gateway/v1/chat/completions

# ❌ 失败 403：charlie 不属于 enterprise-tenant，无权访问 qwen-max
# {"error":"User charlie is not authorized to access model: qwen-max"}
```

### 场景 5：访问未配置的模型

```bash
# 访问不在 model_mapping 中的模型
curl -H "Authorization: Bearer sk-alice-key-1" \
     -H "x-higress-llm-model: unknown-model" \
     http://your-gateway/v1/chat/completions

# ✅ 直接放行：unknown-model 不在配置中，插件跳过鉴权
```

---

## 错误响应

| HTTP 状态码 | 错误类型 | 返回信息 | 场景 |
|------------|---------|----------|------|
| 401 | missing_auth_header | `{"error":"Authorization header is required"}` | 缺少 Authorization 头 |
| 401 | invalid_auth_format | `{"error":"Invalid Authorization header format"}` | Authorization 格式错误（不是 Bearer Token） |
| 401 | invalid_api_key | `{"error":"Invalid API key"}` | API Key 不在 api_key_mapping 中 |
| 403 | access_denied | `{"error":"User {user} is not authorized to access model: {model}"}` | 用户所属租户无权访问该模型 |

---

## 鉴权流程验证表

根据上面的配置示例，各场景的鉴权结果：

| API Key | 用户 | 模型 | 允许的租户 | 用户所属租户 | 结果 |
|---------|------|------|------------|--------------|------|
| sk-alice-key-1 | alice | qwen-turbo | ["*"] | - | ✅ 通过（通配符）|
| sk-alice-key-2 | alice | qwen-turbo | ["*"] | - | ✅ 通过（通配符）|
| sk-alice-key-1 | alice | qwen-plus | ["enterprise-tenant", "premium-tenant"] | enterprise-tenant | ✅ 通过 |
| sk-alice-key-1 | alice | qwen-max | ["enterprise-tenant"] | enterprise-tenant | ✅ 通过 |
| sk-charlie-key-1 | charlie | qwen-turbo | ["*"] | - | ✅ 通过（通配符）|
| sk-charlie-key-1 | charlie | qwen-plus | ["enterprise-tenant", "premium-tenant"] | premium-tenant | ✅ 通过 |
| sk-charlie-key-1 | charlie | qwen-max | ["enterprise-tenant"] | premium-tenant | ❌ 403 拒绝 |
| sk-invalid-key | - | qwen-turbo | - | - | ❌ 401 无效 Key |
| sk-alice-key-1 | alice | unknown-model | (不存在) | - | ✅ 跳过鉴权 |

---

## 构建和部署

### 本地构建

```bash
cd plugins/wasm-go
PLUGIN_NAME=model-auth IMG=quanzhenglong.com/camp/model-auth:latest make build-push
```

### 应用配置

```bash
# 应用 WasmPlugin 配置
kubectl apply -f wasmplugin.yaml

# 查看插件状态
kubectl get wasmplugin model-auth -n higress-system

# 查看插件日志
kubectl logs -n higress-system -l app=higress-gateway --tail=100 | grep model-auth
```

---

## 最佳实践

### 1. 权限设计原则

- **最小权限原则**：只为用户授予访问必要模型的权限
- **租户隔离**：不同租户之间的模型访问应该相互隔离
- **公共资源使用通配符**：对于所有用户都可以访问的模型，使用 `"*"` 通配符

### 2. API Key 管理

- **不要硬编码**：不要将 API Key 硬编码在客户端代码中
- **使用密钥管理系统**：使用 Kubernetes Secret 或外部密钥管理系统存储配置
- **定期轮换**：定期轮换 API Keys，并更新配置
- **多密钥支持**：为每个用户分配多个 API Key，方便轮换和不同应用场景使用
- **密钥隔离**：不同应用或环境使用不同的 API Key，便于追踪和撤销

### 3. 租户管理

- **清晰的命名**：使用有意义的租户名称（如 `finance-dept`, `enterprise-tenant`）
- **定期审计**：定期审查租户成员和权限配置
- **文档化**：维护租户和用户的映射关系文档

### 4. 模型配置

- **分级管理**：将模型按访问级别分类（公共、部门、企业等）
- **渐进式开放**：新模型先限制访问，稳定后再考虑扩大范围
- **未配置模型放行**：利用"未配置模型直接放行"特性，只配置需要控制的模型

### 5. 安全加固

```yaml
# 推荐：使用 Kubernetes Secret 存储敏感配置
apiVersion: v1
kind: Secret
metadata:
  name: model-auth-config
  namespace: higress-system
type: Opaque
stringData:
  config.yaml: |
    api_key_mapping:
      sk-secret-key-1: "user-1"
      sk-secret-key-2: "user-2"
---
# 在 WasmPlugin 中引用 Secret
# （注：这需要 Higress 支持从 Secret 读取配置）
```

### 6. 监控和告警

建议监控以下指标：
- 401/403 错误率（可能表示配置问题或攻击）
- API Key 使用频率（检测异常使用）
- 模型访问模式（了解使用情况）

### 7. HTTPS 保护

⚠️ **始终使用 HTTPS**：API Key 在请求头中传输，必须使用 HTTPS 保护传输安全

---

## 故障排除

### 问题 1：总是返回 401 "Invalid API key"

**可能原因**：
- API Key 不在 `api_key_mapping` 配置中
- Authorization header 格式错误

**解决方案**：
```bash
# 1. 检查 Authorization header 格式（必须是 Bearer Token）
curl -v -H "Authorization: Bearer sk-test" ...

# 2. 检查 API Key 是否在配置中
kubectl get wasmplugin model-auth -n higress-system -o yaml | grep api_key_mapping -A 10

# 3. 查看插件日志
kubectl logs -n higress-system -l app=higress-gateway | grep "API key not found"
```

### 问题 2：返回 403 "Access denied"

**可能原因**：
- 用户不属于模型允许的任何租户
- 租户配置错误

**解决方案**：
```bash
# 1. 确认模型的允许租户列表
# model_mapping[model] 中的租户

# 2. 确认用户属于哪个租户
# workspace_mapping 中查找包含该用户的租户

# 3. 确认租户匹配
# 用户的租户必须在模型的允许租户列表中
```

### 问题 3：某些模型无法访问，但应该可以

**可能原因**：
- 模型名称大小写不匹配
- 配置更新未生效

**解决方案**：
```bash
# 1. 检查模型名称完全匹配（区分大小写）
# x-higress-llm-model header 的值必须与 model_mapping 中的 key 完全一致

# 2. 等待配置生效（通常需要几秒钟）
kubectl get wasmplugin model-auth -n higress-system -o yaml

# 3. 强制重启 gateway pod
kubectl rollout restart deployment higress-gateway -n higress-system
```

### 问题 4：未配置的模型也被拦截

**可能原因**：
- 理解错误，插件应该跳过未配置的模型

**解决方案**：
```bash
# 检查日志，应该看到 "not managed by this plugin, skipping"
kubectl logs -n higress-system -l app=higress-gateway | grep "not managed by this plugin"

# 如果没有此日志，可能是模型名称恰好匹配了配置中的某个模型
```

### 问题 5：配置更新后不生效

**解决方案**：
```bash
# 1. 确认配置已更新
kubectl get wasmplugin model-auth -n higress-system -o yaml

# 2. 查看 WasmPlugin 状态
kubectl describe wasmplugin model-auth -n higress-system

# 3. 重启 gateway
kubectl rollout restart deployment higress-gateway -n higress-system

# 4. 查看日志确认新配置已加载
kubectl logs -n higress-system -l app=higress-gateway --tail=50 | grep "loaded"
```

---

## 版本历史

- **v0.0.12**: 优化 API Key 映射配置结构
  - 修改 `api_key_mapping` 配置格式：从 `apiKey -> username` 改为 `username -> []apiKey`
  - 支持一个用户拥有多个 API Key，便于密钥轮换和多应用场景
  - 内部仍使用反向映射 `apiKey -> username` 保证查询性能
  - `x-api-key-name` header 格式改为 `username/apiKeySuffix`（使用 `/` 分隔）

- **v0.0.11**: 基于租户和工作空间的多级权限模型
  - 移除 `api_key_models` 和 `whitelist` 配置
  - 新增 `model_mapping`、`workspace_mapping`、`api_key_mapping`
  - 支持通配符 `"*"` 允许所有用户访问
  - 未配置的模型直接放行，不进行鉴权
  - 自动设置 `x-api-key-name` header（用户名 + API Key 后8位）

- **v0.0.7**: 初始版本
  - 基于 API Key 和模型白名单的访问控制

---

## 技术支持

如有问题或建议，请通过以下方式联系：

- 📧 Email: admin@higress.io
- 🌐 Website: http://higress.io/
- 💬 GitHub Issues: https://github.com/alibaba/higress/issues

---

## License

Copyright (c) 2022 Alibaba Group Holding Ltd.

Licensed under the Apache License, Version 2.0
