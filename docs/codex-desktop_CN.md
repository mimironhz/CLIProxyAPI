# Codex Desktop / codex-cli 双提供方

把 Codex **内置的 `openai` 提供方**指向本代理，只需切换模型 slug 即可在 ChatGPT
模型与 Grok 模型之间切换。无需给 ChatGPT.app 打 ASAR 补丁，无需自定义
`model_provider`，也无需处理粘滞提供方问题。

| 模型 slug | 上游 | 凭据 |
|---|---|---|
| `gpt-5.6-sol` 等 Codex slug | `chatgpt.com/backend-api/codex` | Codex OAuth（`--codex-login`），或通过透传使用客户端自己的令牌 |
| `grok-4.5`、`grok-4.3` 等 | `cli-chat-proxy.grok.com` | xAI OAuth（`--xai-login`） |

路由通过常规凭据注册表按模型 slug 完成，没有任何 Codex 专用的硬编码。

## 1. 登录

```bash
go build -o cli-proxy-api ./cmd/server

./cli-proxy-api --codex-login   # ChatGPT 账号
./cli-proxy-api --xai-login     # SuperGrok / Grok Build 账号
```

凭据保存在 `auth-dir`（默认 `~/.cli-proxy-api`）。两侧可单独使用；双提供方需要同时添加。

## 2. 配置代理

```yaml
port: 8317
auth-dir: "~/.cli-proxy-api"

# Codex Desktop 会发送自己的 ChatGPT bearer，它并不是代理 API key。
# 将 api-keys 留空会完全关闭入站鉴权，从而直接忽略该 bearer。
# 这样做时请务必只监听 localhost。
api-keys: []
ws-auth: false

routing:
  # 让多轮对话固定使用同一个凭据。
  session-affinity: true
```

启动：

```bash
./cli-proxy-api --config config.yaml
```

## 3. 让 Codex 指向本代理

在 `~/.codex/config.toml` 的根部（位于任何 `[table]` 之前）：

```toml
openai_base_url = "http://127.0.0.1:8317/v1"
# 不要设置 model_provider —— 我们劫持的正是内置的 `openai` 提供方
```

Codex 内置提供方优先使用 Responses **WebSocket** 传输，失败后回退到 HTTP POST。
两者都已支持：

- `GET  /v1/responses`  → WebSocket 升级
- `POST /v1/responses`  → SSE
- 同样的三个路由也挂载在 `/backend-api/codex` 下，供期望原生 ChatGPT 路径的客户端使用。

Grok slug 会自动出现在 Codex 模型列表中：注册表中任何在
`internal/registry/models/codex_client_models.json` 里没有显式条目的模型，都会使用
默认客户端模板发布。

## 4. 验证

```bash
curl -s localhost:8317/v1/models | jq -r '.models[].slug' | grep -i grok

codex -m grok-4.5   exec --skip-git-repo-check "Reply with exactly: pong"
codex -m gpt-5.6-sol exec --skip-git-repo-check "Reply with exactly: pong"
```

然后重启 ChatGPT.app，在两侧各跑一轮以验证 WebSocket 路径。

## 可选：把客户端自己的 ChatGPT 令牌透传到上游

默认情况下 Codex 侧使用 `--codex-login` 保存的凭据。开启下面的开关后，改为转发
**调用方客户端**的 ChatGPT OAuth bearer，使 ChatGPT 用量记在 Codex Desktop 当前
登录的账号上：

```yaml
codex:
  passthrough-client-token: true
```

行为说明：

- 只转发 **JWT 形态** 的 bearer。`api-keys` 中配置的值同样以 bearer 形式发送，因此
  绝不会被误认为 ChatGPT 令牌。
- 透传**仅**作用于 Codex 执行器。xAI 请求始终使用 xAI 自己的凭据。
- 透传生效时，即便所选凭据是 api-key 条目，请求也按 OAuth 处理：会设置
  `Originator`，且客户端的 `Chatgpt-Account-Id` 优先于存储凭据中的值。
- 凭据选择仍然需要选中*某个* Codex 凭据。若不想保存 ChatGPT 登录，可声明一个占位条目：

  ```yaml
  codex-api-key:
    - api-key: "passthrough-placeholder"
      base-url: "https://chatgpt.com/backend-api/codex"
  codex:
    passthrough-client-token: true
  ```

  占位 key 不会被发送，实际使用的是客户端的 bearer。
  `base-url` 是**必填**的：`SanitizeCodexKeys` 会丢弃没有该字段的条目，且不会有任何
  提示——表现为 `/v1/models` 中没有任何 `gpt-*` 模型。

## 推理强度（reasoning effort）

代理原样转发客户端发来的 `reasoning.effort`，只有当该等级不在模型注册表的
levels 列表中时才会做钳制。判断实际发往上游的值，唯一可靠依据是代理自己的调试
日志（`debug: true`）：

```
thinking: original config from request | provider=codex model=gpt-5.6-sol mode=level budget=0 level=max
thinking: processed config to apply    | provider=codex model=gpt-5.6-sol mode=level budget=0 level=max
```

以下三种 Codex 行为经常被误判为代理的 bug，实际都不是：

- **Ultra 不是链路上的 effort 值。** 选择 Ultra 时客户端发送的是
  `reasoning.effort: "max"`，其余部分由客户端侧的多 agent 调度实现，代理只会看到
  `max`。这一点很重要，因为 `/v1/models?client_version=…` 确实会在目录中列出
  `ultra`，而注册表并不接受它——这是上游有意为之（PR #4160 保留了目录中的 `ultra`，
  issue #4463 以“符合预期”关闭：对 Codex 而言 `ultra` 是一种工作模式，而非推理
  等级）。该不一致在 Codex 路径上不可达，只会影响按字面消费这份共享目录的客户端。
- **effort 在一轮对话开始时就已固定**，每个派生出的子 agent 线程也各自持有一份。
  在一轮进行中修改不会影响该轮及其正在运行的子 agent，因此代理会继续记录旧等级，
  直到下一轮开始。
- **输入框无法显示 `max`。** Codex Desktop 的模型控件默认只包含
  `[low, medium, high, xhigh, ultra]`，因此停留在 `max` 的线程会显示为相邻档位
  （“Extra High”），但实际发送的仍是 `max`。

当反馈与代理日志不一致时，先查客户端侧状态，再考虑改动 `internal/thinking/`：

| 位置 | 内容 |
|---|---|
| `~/.codex/state_5.sqlite` | `threads(model, reasoning_effort)`、`thread_spawn_edges` |
| `~/.codex/logs_2.sqlite` | span 字段 `codex.turn.reasoning_effort` / `codex.request.reasoning_effort`，以及仍在进行中的 `turn.id` |
| `~/.codex/sessions/<日期>/rollout-*.jsonl` | 每轮的 `turn_context` |

其中 `codex.*.reasoning_effort` 是 Codex 内部的轮次标签，**不是**请求体：Ultra 轮
次会把它标成 `ultra`，而实际发送的是 `max`。只有代理的 `thinking:` 日志才反映真正
发出的字段。

## Grok 侧与延迟加载工具

Codex 的 `tool_search` 是模型获取**延迟加载**工具的唯一途径——这些工具不在基础工具
列表中，例如 `chrome:control-chrome` 等技能所需的 Node REPL（`mcp__node_repl__js`）。

xAI 拒绝 `tool_search` 这一托管工具类型，因此它被改写为普通 function 转发，而 Grok
的调用会在响应侧还原成 `tool_search_call`（`execution: client`），交由 Codex Desktop
自己的加载器解析。与之配套：

- 从所有转发的工具上移除 `defer_loading`——Grok 没有延迟加载机制，保留该标志的工具
  对模型不可见，模型会无限次重复搜索。
- 客户端加载的工具只存在于 `tool_search_output` 历史条目中，需要从中提取并合并回请求的
  `tools` 数组。
- 整数型工具参数上的整数值浮点数（`timeout_ms: 1000.0`）会被改写为整数，因为 Codex
  按 `i32`/`i64` 解析。

**客户端注意事项：** Node REPL 还受 Codex 自身配置控制。若 `~/.codex/config.toml` 中
存在 `[features] js_repl = false`，Codex 根本不会暴露 `js` 工具，任何代理行为都无济于事。
请设置：

```toml
[features]
js_repl = true
```

## 运维提示

只要设置了 `openai_base_url`，**所有** Codex 模型调用都会经过本代理——包括 ChatGPT 侧
和 Grok 侧。代理不可用时两侧都会失败。回滚方式：删除 `openai_base_url`，Codex 即恢复
直连 ChatGPT。
