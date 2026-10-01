# hindsight-proxy

按 token 路由 bank 的 MCP 代理，位于 Multica agent 与 Hindsight 之间。

它取代了 Hindsight 内置的 `OperationValidatorExtension` 插件方案：插件跑在 Hindsight 进程内，
只能拿到 Hindsight 暴露的钩子，因此归属 tag 可被调用方伪造、工具集只能按 bank 裁剪、派生数据无法控制。
代理是独立进程，能完整掌控请求与响应，这三件事都在这一层解决。

```
Multica agent
   │  Authorization: Bearer <agent-token>
   ▼
┌─────────────────────────────────┐
│  hindsight-proxy         :8890  │
│  · token → bank 路由            │
│  · 按 token 裁剪工具             │
│  · 服务端注入归属 tag（不可伪造） │
│  · bank 自动创建                │
└─────────────────────────────────┘
   │  Authorization: Bearer <上游管理 token>
   ▼
Hindsight  :8888   /v1/default/banks/<bank_id>/...
```

Hindsight 侧同时开启认证，因此代理是唯一入口：agent 拿不到绕过代理直连 8888 的凭据。

## 快速开始

```bash
cp examples/tokens.example.json tokens.json  # 改成真实 token
export HINDSIGHT_TOKEN=<上游管理 token>  # out-of-band 获取，不要提交
docker compose up -d --build             # 镜像未发布到 registry，本地构建
curl -s localhost:8890/healthz
docker inspect --format '{{.State.Health.Status}}' hindsight-proxy   # 期望 healthy
```

## 配置

### 路由表

代理持有一份 JSON 映射表，**热加载**，改完不重启。

```json
{
  "default_bank": "shared",
  "tokens": {
    "<agent-token>": {
      "agent": "Mika",
      "bank": "ops",
      "tools": ["recall", "retain"],
      "read_scope": "shared",
      "description": "运维 agent"
    }
  }
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `default_bank` | 否 | 仅用于省略 `bank` 的条目。**未知 token 不会回落到它**（fail-closed）。 |
| `tokens.<token>` | 是 | 入站 `Authorization: Bearer <token>` 的凭据本身。 |
| `.agent` | 否 | 注入的归属 tag 名，形如 `agent:<agent>`。省略则该调用方写入不带归属 tag，且不能用 `read_scope: "own"`。 |
| `.bank` | 否 | 目标 bank。省略则取 `default_bank`。 |
| `.tools` | 是 | `"*"` 表示不裁剪，或工具名数组。 |
| `.read_scope` | 否 | `"shared"`（默认，看整个 bank）或 `"own"`（只看自己写的）。 |
| `.description` | 否 | 仅文档用，会写进启动日志。 |

配置非法（未知工具名、非法 bank id、`own` 但无 `agent`、空表）时**启动即失败**，运行中改坏则保留上一份可用配置并打 error 日志。

### 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8890` | 监听地址 |
| `CONFIG_PATH` | `/etc/hindsight-proxy/tokens.json` | 路由表路径 |
| `HINDSIGHT_URL` | `http://127.0.0.1:8888` | Hindsight 基址 |
| `HINDSIGHT_TOKEN` | 无（**必填**） | 上游管理 token |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## 加一个 agent

1. 生成 token：`openssl rand -hex 24`
2. 往路由表 `tokens` 里加一条，指定 `agent`、`bank`、`tools`、`read_scope`
3. **不用重启**：代理每 2 秒轮询一次，改动自动生效（日志有 `routing table reloaded`）
4. 把 token 配到该 agent 的 `mcp_config`：

```json
{
  "mcpServers": {
    "hindsight": {
      "type": "http",
      "url": "http://<proxy-host>:8890/mcp",
      "headers": { "Authorization": "Bearer <该 agent 的 token>" }
    }
  }
}
```

## 新增 bank

**不需要手工建库**。调用方第一次请求某个不存在的 bank 时，代理会自动 `PUT /v1/default/banks/{bank}` 再继续，并把结果缓存住，后续请求不再探测。

所以"新增 bank"就是在路由表里把某个 token 的 `bank` 写成新名字，然后 `mkdir` 那一步由代理完成。

## 工具

默认暴露 6 个工具。工具面的字符数有三种口径，含义不同，这里都列出（实测值）：

| 口径 | 字符数 | 说明 |
| --- | --- | --- |
| description + schema | 2626 | issue 验收标准 4 的字面口径 |
| 再加工具名（`ToolSurfaceSize()`） | 2689 | 测试与 `make check` 用的口径，即上面这条加上 name |
| `tools/list` 实际下发字节 | 3441 | 最接近 issue「控制上下文膨胀」本意的口径 |

前两个口径**低于 3000**；第三个（含 MCP safety annotations）**高于 3000**。annotations 由 MCP SDK
在序列化时补全（`readOnlyHint` / `additiveHint` / `openWorldHint` / `idempotentHint`），每个工具约 66–91
字符，6 个共 421 字符，是不可省略的协议字段。若要求按实际下发字节也 < 3000，需要进一步精简
`recall` / `reflect` 的 schema（这两个各约 600 / 500 字符，是主要开销）。

| 工具 | 说明 |
| --- | --- |
| `retain` | 写记忆。同步，返回即可查。 |
| `recall` | 语义检索 |
| `reflect` | 综合作答 |
| `list_memories` | 平铺浏览（带分页上限） |
| `get_memory` | 按 id 取单条 |
| `list_tags` | 查看已有 tag |

Hindsight 原生 36 个工具中的破坏性工具（`delete_bank` / `clear_memories` / `delete_document`）
**不在本代理的表面上**：即使 token 配 `"tools": "*"` 也够不到，因为这些工具从未被注册。

## 归属不可伪造

写入时由**代理**注入 `agent:<名字>`，名字取自路由表而非请求体。

调用方传入的所有 `agent:*` tag 在出站前被剥离（大小写与首尾空格都会归一化后比对），
其余 tag 正常保留。因此调用方无法把自己写成别人。

## 读隔离

`read_scope` 是**每个调用方一行配置**，不是全局二选一：

- `"shared"`（默认）：recall / list / reflect 不过滤，与现状一致。
- `"own"`：只看自己写的。

`own` 的实现不只是在请求上带 tag 过滤，还在响应上再筛一次。原因是 Hindsight 的 tag 匹配默认
"any（含无 tag）"，一条无归属 tag 的记忆会穿过请求侧过滤；派生内容（observation）也不保证继承
调用方的 tag。所以请求侧过滤 + 响应侧过滤都做，才能保证隔离。

`own` 下：调用方无法通过点名别人的 tag 来扩大自己的可见范围；`get_memory` 对非自己的记忆返回
"不存在"而非"无权限"，避免探测。

## 验收对照

| # | 验收标准 | 覆盖测试 |
| --- | --- | --- |
| 1 | 未知 token → 401；已知 token 按其 `bank` 路由 | `TestUnknownTokenIsRejectedWithoutFallingBack`、`TestKnownTokenRoutesToItsBank` |
| 2 | 两个 token 指向不同 bank，写入互不可见 | `TestTwoTokensOnDifferentBanksCannotSeeEachOther` |
| 3 | 伪造 `agent:别人` 被剥离，库里只有真实归属 | `TestCallerSuppliedAgentTagsAreStrippedAndReplaced` |
| 4 | 工具按 token 裁剪；6 个工具合计 < 3000 字符 | `TestToolListIsTrimmedPerToken`、`TestToolSurfaceStaysWithinItsBudget`（按 description+schema 口径，见上文三种口径） |
| 5 | 映射表改动不重启即生效 | `TestRoutingTableChangeTakesEffectWithoutRestart` |
| 6 | 目标 bank 不存在时自动创建 | `TestBankIsCreatedOnDemand` |

另有一组不在验收清单内、但属于方案核心价值的测试：读隔离
（`TestReadScopeOwnHidesOtherWritersInTheSameBank`、
`TestReadScopeTagFilterCannotBeWidenedByNamingAnotherAgent`）、
破坏性工具不暴露（`TestDestructiveToolsAreNeverExposed`）、
调用方 token 不会被转发给上游（`TestCallerTokenIsNeverForwardedUpstream`）。

```bash
go test ./... -race
```

真实端到端（起一个真的 Hindsight 跑全套）见 [`docs/verification.md`](docs/verification.md) 与
[`scripts/verify_e2e.sh`](scripts/verify_e2e.sh)。

## 运维

```bash
docker compose logs -f hindsight-proxy     # JSON 日志
curl -s localhost:8890/healthz             # {"status":"ok","upstream":"ok","rules":4}
```

`/healthz` 在 Hindsight 不可达时返回 503 并说明原因；容器 healthcheck 用的就是它。

要换上游 token：改环境变量后 `docker compose up -d` 重建容器（只有这一个值需要重启，路由表不用）。

## 上游接口

代理直接转 REST，不实现 MCP-over-Hindsight：

| 用途 | 方法与端点 |
| --- | --- |
| 建 bank | `PUT /v1/default/banks/{bank_id}` |
| 写记忆 | `POST /v1/default/banks/{bank_id}/memories` |
| 检索 | `POST /v1/default/banks/{bank_id}/memories/recall` |
| 综合作答 | `POST /v1/default/banks/{bank_id}/reflect` |
| 列记忆 | `GET /v1/default/banks/{bank_id}/memories/list` |
| 取单条 | `GET /v1/default/banks/{bank_id}/memories/{memory_id}` |
| 列 tag | `GET /v1/default/banks/{bank_id}/tags` |

bank_id 一律由 token 解析得出。请求体里若带 `bank_id`，该字段被丢弃——写入的出站请求是按白名单
字段重建的，`bank_id` 不在白名单里，因此调用方无法指定 bank。
