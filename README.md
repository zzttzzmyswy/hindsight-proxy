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
| description + schema | 2736 | issue 验收标准 4 的字面口径 |
| 再加工具名（`ToolSurfaceSize()`） | 2799 | 测试与 `make check` 用的口径，即上面这条加上 name |
| `tools/list` 实际下发字节 | 3551 | 最接近 issue「控制上下文膨胀」本意的口径 |

上表是**紧凑序列化**（`json.Marshal` 默认，即代理实际发出的字节）。若用 Python
默认的带空格分隔符统计，前两档会变成 **2916 / 2979**——数字不同但 payload 完全相同，
只是分隔符；引用数字时请注明口径。（v0.1.4 的 `document_id` 让紧凑三档各 +110 字符。）

前两个口径**低于 3000**；第三个（含 MCP safety annotations）**高于 3000**。annotations 由 MCP SDK
在序列化时补全（`readOnlyHint` / `additiveHint` / `openWorldHint` / `idempotentHint`），每个工具约 66–91
字符，6 个共 421 字符，是不可省略的协议字段。若要求按实际下发字节也 < 3000，需要进一步精简
`recall` / `reflect` 的 schema（这两个各约 600 / 500 字符，是主要开销）。

| 工具 | 说明 |
| --- | --- |
| `retain` | 写记忆。同步，返回即可查。items 里可带 `document_id`，见下。 |
| `recall` | 语义检索 |
| `reflect` | 综合作答 |
| `list_memories` | 平铺浏览（带分页上限） |
| `get_memory` | 按 id 取单条 |
| `list_tags` | 查看已有 tag |

Hindsight 原生 36 个工具中的破坏性工具（`delete_bank` / `clear_memories` / `delete_document`）
**不在本代理的表面上**：即使 token 配 `"tools": "*"` 也够不到，因为这些工具从未被注册。

### `document_id`：更正一条已存的记忆

`retain` 的每个 item 可带 `document_id`（稳定键，例如 `"env:nas-hindsight-port"`）。
**同一个 `document_id` 再写一次会替换旧版本**（上游 upsert：库中只留一个 document，旧值派生出的
memory 被清掉），而不是并排再堆一条。

这是代理唯一的「更正」手段：按原设计**不暴露任何删除工具**，所以一条过时的事实如果没有稳定的
`document_id`，就会和它的新版本一起被 recall 出来，越积越多。

约定：**会变的事实**（端口、版本、路径等环境事实）给一个稳定键，值变了就用同一个键重写；
**只增不改的事实**（决策、偏好、踩坑记录）不必给键。是否给键由 agent 判断，代理不强制。

不带 `document_id` 时该字段不会发到上游（`omitempty`），一次写入就是一个新 document——
所以代理不会替调用方凭空造一个键，否则所有无键写入会塌到同一个 document 上。

## 记忆用法协议（由代理的 MCP instructions 下发）

代理在 MCP `initialize` 握手的 `instructions` 字段里下发用法协议，内容是固定的英文段落
（`internal/mcpserver/server.go` 的 `usageProtocol`），跟在按调用方生成的路由说明后面：

> Usage: call recall with the task topic before starting work. Before finishing, retain facts
> worth knowing next time: user preferences, decisions and their reasons, environment facts
> (hosts, paths, versions), and pitfalls with their fixes. Do not retain transient progress,
> secrets, or tokens. Give facts that can change a stable document_id (e.g.
> "env:nas-hindsight-port") and reuse it when the fact changes.

**为什么放这里**：接入本代理的 agent 各自的指令里都没有「何时调用 Hindsight」的说明，
于是工具虽在却没有东西驱动 agent 去用（上线后某个 bank 长期是 0 条，直到测试才写入第一条）。
放在代理的 `instructions` 里，所有已接入的 agent **一处生效**，不需要逐个改各家的 agent 指令；
且 `instructions` 不计入工具面预算。

调用方无需做任何事，任何 MCP 客户端握手时都会收到。

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

### 写入后不要立刻用 own token 轮询（重要）

Hindsight 是**异步**把 document 抽取成 memory 的：实测 document 的 `memory_unit_count` 从 0 变正
需要约 10–15s。所以写完立刻 `recall` 返回空是**合法**的，不代表代理有问题。

要断言「own 调用方能看到自己写的」，**不要用 own token 反复轮询等结果**。正确做法是用**上游管理
token** 等落库（`GET /v1/default/banks/{bank}/memories/list` 直到目标 tag 计数 > 0），确认抽取完成后
再用 own token 读一次。`scripts/verify_e2e.sh` 的 read_scope 段就是这么做的：读路径只被观察一次，
且发生在数据稳定之后。

> 说明：关于「轮询读本身会污染归属 tag」这一条，我们在 13.24 上做过对照实验（同一探针文本，
> 一侧用 own token 轮询、一侧静默等待，各 3 次），**未能复现**——两组最终 tag 都正确。该现象在
> NAS 上被观察到，但成因尚未确认，因此这里不作为已证结论；改成上游等待是无论成因都成立的做法。

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
调用方 token 不会被转发给上游（`TestCallerTokenIsNeverForwardedUpstream`）、
`retain` 的 `document_id` 透传与 `omitempty`（`TestRetainPassesDocumentIDUpstream`、
`TestRetainOmitsDocumentIDWhenNotGiven`、`TestRetainSchemaAdvertisesDocumentID`）、
用法协议随握手下发（`TestInitializeInstructionsCarryRoutingAndUsageProtocol`）。

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

**记忆用法协议不需要部署动作**：它在 MCP 握手里下发，代理重启或不动都不影响——agent 每次
握手都会拿到当前版本。改协议文本要改代码并发新版本（`usageProtocol` 是编译期常量）。

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
