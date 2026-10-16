# iotserver-http-driver

iotserver 物联网平台的 **HTTP 协议驱动**：设备 / 网关用 HTTP POST 上报数据，
用轮询接口取平台下发的命令。

```
设备 ──HTTP POST /ingest/{sn}──► 驱动 ──gRPC──► 平台核心
     ◄──GET /ingest/{sn}/commands──┘
```

## 一、平台侧怎么用

1. **构建镜像**（换成你的仓库地址）：

   ```bash
   make push IMAGE=registry.example.com/iotserver-http-driver:v1.0.0
   ```

2. **平台「设备接入 → 驱动管理」**：添加驱动（镜像填上面的地址），
   到「驱动实例」新建实例，**自定义参数**填：

   ```json
   {
     "ListenAddr": "0.0.0.0:9090",
     "Token": "my-shared-secret",
     "AutoCreateDevice": false
   }
   ```

3. **端口映射**：`ListenAddr` 的端口（默认 9090）是设备上报端口，
   在驱动实例的 Docker 参数里映射，例如 `-p 9090:9090`。

4. 平台「设备管理」新建设备并绑定该驱动实例，设备序列号（SN）用于 URL 路径。

## 二、设备侧怎么接

### 上报属性

```bash
curl -X POST http://<驱动主机>:9090/ingest/device-001 \
  -H 'Content-Type: application/json' \
  -H 'X-Token: my-shared-secret' \
  -d '{"temp": 23.5, "rpm": 1450}'
# → {"ok":true,"reported":2}
```

### 上报事件

```bash
curl -X POST http://<驱动主机>:9090/ingest/device-001/events \
  -H 'Content-Type: application/json' \
  -H 'X-Token: my-shared-secret' \
  -d '{"alarm": {"code": "E01", "msg": "超温"}}'
```

### 取平台下发的命令（轮询）

```bash
curl http://<驱动主机>:9090/ingest/device-001/commands -H 'X-Token: my-shared-secret'
# → {"ok":true,"commands":[{"cmd":"set","data":{"temp":25}}]}
```

取走即清空（每条命令只投递一次）。设备建议 5~30 秒轮询一次。

### 健康检查

```bash
curl http://<驱动主机>:9090/health
# → {"ok":true,"time":1735689600000}
```

## 三、接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/ingest/{deviceSn}` | 上报属性（body 为属性键值对） |
| POST | `/ingest/{deviceSn}/events` | 上报事件 |
| GET | `/ingest/{deviceSn}/commands` | 轮询取平台下发命令 |
| GET | `/health` | 健康检查 |

鉴权：自定义参数 `Token` 非空时所有 `/ingest/*` 请求必须带 `X-Token: <token>`。

## 四、自定义参数

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `ListenAddr` | `0.0.0.0:9090` | HTTP 监听地址 |
| `Token` | 空（不校验） | 上报鉴权密钥，**生产环境务必设置** |
| `AutoCreateDevice` | `false` | 未注册设备上报时是否自动在平台创建设备 |

## 五、适用场景

HTTP 驱动适合**主动上报型**设备：

- 电表 / 水表等定时上报的仪表（自带 4G 模组，往固定 URL POST）；
- 网关 / DTU 汇聚多个子设备后统一上报；
- 第三方系统对接（对方只会发 HTTP）。

**不适用**需要实时下发命令的场景：HTTP 无法主动推送，下行只能靠设备轮询，
有延迟。这类场景用 MQTT 或 TCP 驱动。

## 六、本地开发

```bash
go mod tidy
go build ./...
```

驱动依赖平台下发的配置（`/etc/driver/res/configuration.toml`），**不能脱离平台单独跑**。

## 自动构建镜像（GitHub Actions）

仓库自带 workflow，**打 tag 即自动构建并推送镜像到 ghcr.io**，用内置 `GITHUB_TOKEN`
认证，不需要配置任何 secret：

```bash
git tag v1.0.0
git push origin v1.0.0
```

产出镜像：`ghcr.io/lmgateways/<仓库名>:1.0.0`（同时打 `1.0` 和 `latest`）。

推到 `main` 或提 PR 时**只构建不推送**，用来提前发现构建失败。

> ⚠️ ghcr 的包**默认是私有的**，第一次推送后需要手动改一次可见性：
> 打开 `https://github.com/orgs/LMGateways/packages` → 选中该包 →
> Package settings → Change visibility → Public。
> 否则平台拉取镜像会失败（或者你需要在平台上配置 ghcr 凭据）。
>
> 改过可见性之后，后续版本不用再改。

## 本地构建（不走 CI 时）

```bash
make build IMAGE=<你的镜像地址>    # 只构建
make push  IMAGE=<你的镜像地址>    # 构建并推送
```
