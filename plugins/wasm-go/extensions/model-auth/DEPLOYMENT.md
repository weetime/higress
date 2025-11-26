# Model Auth Plugin 部署指南

本文档介绍如何将 model-auth 插件部署到 Higress 网关。

## 构建信息

- **插件名称**: model-auth
- **镜像地址**: quanzhenglong.com/camp/model-auth:20251126-171240-7c4899ad
- **构建时间**: 2025-11-26 17:12:40
- **Commit ID**: 7c4899ad

## 快速开始

### 1. 构建插件

```bash
cd /path/to/higress/plugins/wasm-go
PLUGIN_NAME=model-auth make build
```

### 2. 运行单元测试

```bash
cd extensions/model-auth
go test -v
```

测试包含以下场景：
- ✅ 有效的 API Key 认证
- ✅ 无效的 API Key 拒绝
- ✅ 缺失 API Key 拒绝
- ✅ 配置验证（缺少 api_key）

### 3. 构建并推送镜像

```bash
cd /path/to/higress/plugins/wasm-go
PLUGIN_NAME=model-auth REGISTRY=quanzhenglong.com/camp/ make build-push
```

## 部署到 Higress

### 方式一：使用 kubectl 部署

1. 创建 WasmPlugin 资源：

```bash
kubectl apply -f extensions/model-auth/wasmplugin.yaml
```

2. 验证部署状态：

```bash
kubectl get wasmplugin -n higress-system
kubectl describe wasmplugin model-auth -n higress-system
```

### 方式二：自定义配置

创建自定义的 WasmPlugin 配置文件：

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: model-auth
  namespace: higress-system
spec:
  # 全局配置：对所有请求生效
  defaultConfig:
    api_key: "your-production-secret-key"
  
  # 路由级别配置：针对特定 Ingress 生效
  matchRules:
  - ingress:
    - default/api-service
    config:
      api_key: "api-service-key-123"
  
  - ingress:
    - default/admin-service
    config:
      api_key: "admin-service-key-456"
  
  # 域名级别配置：针对特定域名生效
  - domain:
    - "*.api.example.com"
    config:
      api_key: "api-domain-key-789"
  
  # 镜像地址
  url: oci://quanzhenglong.com/camp/model-auth:20251126-171240-7c4899ad
```

## 验证插件功能

### 1. 成功认证（200 OK）

```bash
curl -H "X-Model-Auth-Key: test-secret-key-123" \
     http://your-gateway-address/api/test
```

预期返回：正常的后端响应

### 2. 认证失败 - 缺少密钥（401 Unauthorized）

```bash
curl http://your-gateway-address/api/test
```

预期返回：
```
HTTP/1.1 401 Unauthorized
Content-Type: text/plain

Unauthorized: X-Model-Auth-Key header is required
```

### 3. 认证失败 - 密钥错误（403 Forbidden）

```bash
curl -H "X-Model-Auth-Key: wrong-key" \
     http://your-gateway-address/api/test
```

预期返回：
```
HTTP/1.1 403 Forbidden
Content-Type: text/plain

Forbidden: Invalid API key
```

## 查看插件日志

```bash
# 查看 Higress Gateway Pod 日志
kubectl logs -n higress-system -l app=higress-gateway --tail=100 -f

# 过滤 model-auth 相关日志
kubectl logs -n higress-system -l app=higress-gateway --tail=100 | grep model-auth
```

## 配置说明

### 配置项

| 配置项 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| api_key | string | 是 | 预期的 API Key 值 |

### 配置优先级

1. 域名级别配置（domain）- 优先级最高
2. 路由级别配置（ingress）- 优先级中等
3. 全局配置（defaultConfig）- 优先级最低

## 故障排查

### 问题：插件未生效

1. 检查 WasmPlugin 资源状态：
```bash
kubectl get wasmplugin model-auth -n higress-system -o yaml
```

2. 检查镜像是否可访问：
```bash
docker pull quanzhenglong.com/camp/model-auth:20251126-171240-7c4899ad
```

3. 检查 Higress Gateway 日志：
```bash
kubectl logs -n higress-system -l app=higress-gateway --tail=200
```

### 问题：认证总是失败

1. 确认配置的 api_key 是否正确
2. 确认请求头名称为 `X-Model-Auth-Key`（大小写敏感）
3. 检查插件日志中的错误信息

### 问题：镜像拉取失败

1. 确认镜像仓库地址是否正确
2. 如果是私有仓库，需要配置 imagePullSecrets：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: registry-secret
  namespace: higress-system
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: <base64-encoded-docker-config>
```

## 更新插件

### 1. 重新构建镜像

```bash
cd /path/to/higress/plugins/wasm-go
PLUGIN_NAME=model-auth REGISTRY=quanzhenglong.com/camp/ make build-push
```

### 2. 更新 WasmPlugin 配置

更新 `wasmplugin.yaml` 中的镜像标签，然后应用：

```bash
kubectl apply -f extensions/model-auth/wasmplugin.yaml
```

### 3. 重启 Higress Gateway（可选）

```bash
kubectl rollout restart deployment/higress-gateway -n higress-system
```

## 卸载插件

```bash
kubectl delete wasmplugin model-auth -n higress-system
```

## 技术架构

### 插件工作流程

```
客户端请求
    ↓
[Higress Gateway]
    ↓
[model-auth Plugin]
    ↓
1. 提取 X-Model-Auth-Key 请求头
2. 与配置的 api_key 比对
3. 验证通过 → 继续请求
   验证失败 → 返回 401/403
```

### 性能特性

- **语言**: Go (编译为 WebAssembly)
- **运行时**: Envoy Proxy WASM Runtime
- **性能开销**: < 1ms（内存操作）
- **资源占用**: WASM 文件大小约 5.3MB

## 最佳实践

1. **密钥管理**：使用 Kubernetes Secret 存储 API Key
2. **日志记录**：启用详细日志以便排查问题
3. **多级配置**：根据不同服务使用不同的 API Key
4. **监控告警**：监控认证失败率，及时发现异常
5. **定期更新**：定期更新 API Key，提高安全性

## 下一步

- 添加更多认证方式（JWT、OAuth2 等）
- 支持多个 API Key（白名单）
- 集成外部认证服务
- 添加速率限制功能
- 支持认证缓存以提高性能

