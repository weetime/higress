### 构建

VERSION=v0.0.5 make build-push

### 配置说明

本插件对命中 `matchRules` 的路由进行 API Key 鉴权。鉴权采用 **OR 语义**：

```
放行 ⟺  apikey ∈ allow_apikeys   或   user ∈ allow_workspace_projects
```

#### 全局配置（defaultConfig）

```jsonc
{
  // 租户 -> 用户列表
  "workspace_users": { "ai-for-deployer": ["admin", "xiaoma"] },
  // 项目 -> 用户列表
  "project_users":   { "project-a": ["admin", "user1"] },
  // 用户 -> 该用户持有的 apikey 列表（内部转为 apikey -> user 映射）
  "user_apikeys": {
    "admin":     ["sk-aaa", "ak-bbb"],
    "username2": ["sk-ccc"]
  },
  // 可选。用于强制指定唯一的凭证来源 header。
  // 不配置时，插件自动按优先级兼容多种协议：
  //   x-api-key / x-authorization / anthropic-api-key（Anthropic 风格）
  //   -> Authorization: Bearer <token>（OpenAI 风格）
  "auth_header_name": "Authorization"
}
```

> 双协议兼容：无需任何额外配置，插件即可同时受理 OpenAI（`Authorization: Bearer <key>`）
> 与 Anthropic（`x-api-key: <key>`）两种请求，提取到的 key 统一用于鉴权，并回写
> `x-mse-consumer` / `x-api-key-name` 供下游统计、配额等插件按身份消费。

#### 路由配置（matchRules）

每个 `matchRule` 对应一条 ai-route（即一个模型）。两个授权维度可单独或组合使用，
至少配置其一，否则该路由 deny all：

```jsonc
{
  "rule_name": "infer-357cefd0-c54d-4d8b-9637-2beb7006c0e4",
  // 维度一：按 租户/项目 授权（用户属于哪个项目命中即放行，支持 * 通配）
  //   "*:*"                      公开，所有已认证 apikey 放行
  //   "tenant1:*"                tenant1 下所有用户放行
  //   "tenant1:project-a,project-b"  指定项目内的用户放行
  "allow_workspace_projects": ["tenant1:project-a"],
  // 维度二：直接把本路由（=模型）授权到具体 apikey，命中即放行，
  //   不要求该 apikey 对应用户属于上面的项目。
  "allow_apikeys": ["sk-aaa", "sk-ccc"]
}
```

- 仅配 `allow_workspace_projects`：按项目维度鉴权（原有行为，向后兼容）。
- 仅配 `allow_apikeys`：该模型只对白名单内的 apikey 开放。
- 两者都配：满足任一即放行。
- 两者都为空：该路由拒绝所有访问（deny all）。
