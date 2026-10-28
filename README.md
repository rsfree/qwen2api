<p align="center">
  <b style="font-size:56px">Q</b>
</p>

<h1 align="center">qwen2api</h1>

<p align="center">
  <b>把 chat.qwen.ai 网页端变成 OpenAI 兼容 API 的多账号网关 · 附 Web 控制台</b><br>
  多账号池 · 轮换与冷却 · 思考档位（自动/强制/快速）· 流式 / 非流式 · 三协议前门 · 「深度思考」全程透出
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22-00ADD8?style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic%20%7C%20Responses-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="Deps" src="https://img.shields.io/badge/Dependencies-zero-brightgreen?style=flat-square">
</p>

---

> [!NOTE]
> **契约代次披露**：本仓的上游契约是 **2026-10-28 重新逆向**的（浏览器一手抓包）。
> 上一版公开契约（`rsfree/qwen`，2026-09-24）在**鉴权方式、`version` 头、模型端点**三处已失效，
> 差异逐条列在 [`docs/UPSTREAM.md` §9](docs/UPSTREAM.md)。上游随时会再变，
> 通道稳定性不保证；账号风控后果由使用者自行承担。

> ⚠️ **仅限自用账号**：本项目不做批量小号分发额度、不提供任何形式的付费中转。请只用你自己的账号。

## 核心能力

| 能力 | 说明 |
|---|---|
| 🔄 **多账号池** | 凭据池；选号 =「最闲 + 最久未用」，单号在途限流，避免一个号被连打 |
| 🔑 **两种凭据都吃** | ① 粘贴 Bearer token（F12 复制）；② 直接给邮箱+密码——网关自己走 `signin → refresh` 换 access_token，并在 401 时**自动重登续期** |
| 🛡️ **分级冷却** | 401 ⇒ 长冷却（默认 1800s）；额度耗尽 / 429 / 风控 ⇒ 软冷却指数退避（封顶 1h）；状态落盘、重启不丢 |
| 🧠 **思考档位** | 请求级 `thinking: auto / thinking / fast`，逐字对齐网页端「自动 / 强制 / 快速」三档；兼容 `reasoning_effort`（low/medium/high） |
| 💭 **真思考透出** | 上游「深度思考」的分步摘要经 diff 后走 `reasoning_content`，不是编的；`usage.output_tokens_details.reasoning_tokens` 一并透传 |
| ⚡ **流式 + 非流式** | 出站恒 `stream:true`，非流式由本地聚合（只有一条上游分支，行为可预测）；`stream_options.include_usage` 支持 |
| 🤝 **三协议前门** | `POST /v1/chat/completions`（OpenAI）· `POST /v1/messages`（Anthropic Messages，Claude Code 可接）· `POST /v1/responses`（Responses 形态） |
| 🎨 **图片生成/编辑** | `POST /v1/images/generations`（文生图 + **图生图**）与 `POST /v1/images/edits`（multipart 前门）。字段集对齐 image-adapter 规范；`image` 支持 URL / data URI / 裸 base64，**参考图自动走 `getstsToken` → OSS V4 签名 PUT 上传** |
| 🎬 **视频生成** | `POST /v1/videos/generations` 提交异步任务 + `GET /v1/videos/{id}` 轮询成片。支持 720P/1080P、5/10/15 秒、画幅与音轨开关 |
| 🖼️ **媒体产物** | 图片返回带签名的 CDN 直链（`cdn.qwenlm.ai`），可选 `b64_json` 内联；视频返回 `tsk_` 任务的产物 URL |
| 📄 **文件解析** | `POST /v1/files` 上传（PDF/txt/任意文档）→ 在 chat 的 content part 里引用 `file_id`，模型直读内容。**实测**：95 字节 txt 里埋「紫色大象 42」，提问后原样答出 |
| 🔍 **联网搜索** | 上游 `feature_config.auto_search` 默认开启；`thinking: auto` + 明确要求即可检索当日新闻并附来源链接 |
| 🔁 **失败换号** | 只对「可证明未受理」的失败换号重试（401/额度/429/风控）；5xx 与超时**不重试**，绝不赌重复提交 |
| 🧾 **真实 usage** | 输入/输出/思考 token 全部来自上游，不做本地估算（`count_tokens` 除外，那里明写是估算） |
| 🖥️ **Web 控制台** | 内嵌单页（零外部依赖）：账号池状态 · 请求流水 · 首字/耗时/token · 在线试跑（流式）· 一键重置账号 |
| 🪶 **零依赖** | 只用 Go 标准库，`CGO_ENABLED=0` 静态二进制（约 8.8MB），多架构镜像 |

## 快速开始

```bash
# 1. 配置：把浏览器里的 Bearer token 填进去
cp config.example.json config.json
#    F12 → Network → 任一 /api/v2 请求 → 复制 Authorization 里的那串

# 2. 跑起来
go run ./cmd/server -config config.json
#    或 docker compose up -d --build

# 3. 验证
curl -s localhost:8400/readyz
curl -s localhost:8400/v1/chat/completions \
  -H 'Authorization: Bearer sk-qwen-change-me' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.7-plus","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

控制台：<http://localhost:8400/panel/>（若配置了 `api_keys`，在右上角填入）。

### 容器部署（镜像）

```bash
docker run -d --name qwen2api -p 8400:8400 \
  -v $PWD/data:/app/data \
  -e QWEN2API_CONFIG=/app/data/config.json \
  ghcr.io/rsfree/qwen2api:latest
```

配置有两条路，任选：

| 方式 | 做法 | 适用 |
|---|---|---|
| 挂配置文件 | `-v $PWD/data:/app/data` + `QWEN2API_CONFIG=/app/data/config.json`（或 `-config` 参数） | 账号多、要用 `pool` 这类结构化配置 |
| 纯环境变量 | 只给 `QWEN2API_ACCOUNTS` / `QWEN2API_API_KEYS`，不挂任何文件 | 平台托管、配置即密文 |

三个容易踩的点：

- **配置路径必须显式给**。镜像里既没有 cwd 的 `config.json`、也不该把配置塞进 `/usr/local/bin` 旁边。
  不给的话服务会**正常启动但 0 账号**（不是启动失败，更难发现）——
  所以启动日志第一行就是「配置来源：…」，先看它。
- **容器内是 `uid 10001` 的非 root**。挂载目录要可写（`chmod 777` 或 `chown 10001`），
  否则 `state_file` 写不进去、配额计数重启就丢。
- **配置挂载用 `:ro` 可以，但 `data/` 不行**——`state_file` 要写。

### 凭据怎么拿（两种，任选）

**A. 账号密码（最省事，推荐）**

```json
"accounts": [{ "name": "me", "email": "you@example.com", "password": "明文密码" }]
```

网关启动时自动 `POST /api/v2/auths/signin`（密码走 sha256）→ 用返回的 cookie 调
`auth.qwen.ai/api/v2/auths/refresh` → 拿到 `access_token` 当 Bearer。凭据失效会自动重登。

**B. 直接粘 token**

1. 浏览器登录 <https://chat.qwen.ai>；
2. F12 → Network → 随便点开一个 `/api/v2/...` 请求；
3. 复制 `Authorization: Bearer eyJ...` 里的 `eyJ...` 填进 `accounts[].token`。

> 🔴 **两种凭据不通用**：`signin` 签发的 cookie token 只能读（`/models` 200），
> 调写端点（`chats/new`、`completions`）会 `Unauthorized`；写端点必须用 `refresh` 换来的
> `access_token`。本仓用 `cookie:` / `bearer:` 前缀区分，裸 token 会在启动时探测形态。
> 也兼容完整 cookie jar（含 `token=` 或 `;`）。

## API

### `POST /v1/chat/completions`

```bash
curl -N localhost:8400/v1/chat/completions \
  -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{
    "model": "qwen3.7-plus",
    "thinking": "auto",
    "stream": true,
    "stream_options": {"include_usage": true},
    "messages": [{"role":"user","content":"用一句话解释量子纠缠"}]
  }'
```

扩展字段（不传就用服务端默认）：

| 字段 | 取值 | 说明 |
|---|---|---|
| `thinking` | `auto` / `thinking` / `fast` | 思考档位；`off`/`none` 归一到 `fast` |
| `reasoning_effort` | `low` / `medium` / `high` | 兼容写法，映射到 fast / auto / thinking |

响应里 `choices[0].message.reasoning_content`（非流式）或 `delta.reasoning_content`（流式）
就是网页端的「深度思考」摘要。

### `POST /v1/images/generations`

字段集对齐 image-adapter 的规范形态（文生图与图生图同一个端点，靠 `image` 分流）：

```bash
curl -s localhost:8400/v1/images/generations \
  -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{"prompt":"a cat wearing a beret, flat illustration","size":"16:9"}'
```

```json
{"created":1793185538,"data":[{"url":"https://cdn.qwenlm.ai/output/.../t2i/....png?key=<JWT>"}]}
```

| 字段 | 说明 |
|---|---|
| `prompt` | 无 `image` 时必填 |
| `image` | `str` 或 `str[]`（URL / data URI / 裸 base64），最多 20 张。非空即走**图生图**：先逐张上传到 qwen 自有 OSS（STS → V4 签名 PUT），再以 `chat_type=image_edit` 提交 |
| `mask` | 没有 `image` 时传了就是 400（面门收下但不转发——上游无 mask 语义，绝不假装 inpaint） |
| `n` | 非法值归一为 1；上限 4（每张都是一次独立上游调用 + 一次配额） |
| `size` | `1024x1024` / `1:1` / `auto` 都收，内部映射成上游 5 档比例 |
| `response_format` | `url`（默认）/ `b64_json`；不认识的值视为未指定，不报错 |
| `quality` / `style` / `user` | 收下但不影响上游（qwen 没有这些概念，不假装支持） |

> 🔴 `prompt` **原样发给上游**，不要自己加「请画一张图：」之类的包装。t2i 是显式类型，
> 上游按 `chat_type` 分派；被改写的 prompt 会让它静默空转（实测 200s 无内容）。
>
> ⚠️ **产物链接的两个坑**（实测，本仓已兜住）：
> 1. **刚生成的那几秒 CDN 还没转存完** —— 立刻下会得到 404 + 226 字节 HTML「死链」。
>    `b64_json` 模式带退避重试；`url` 模式原样转出（image-adapter 同口径）。
> 2. **下载要带 `Referer: https://chat.qwen.ai/`**（视频产物尤其必需），否则同样是 404。
>    网关的图片 b64 下载与视频 `/content` 代理都自动带。
> 3. 判真假看 **magic bytes**，不看 `Content-Type`（真图可能回 `application/octet-stream`）。

### `POST /v1/videos/generations` + `GET /v1/videos/{id}`

```bash
# 提交
curl -s localhost:8400/v1/videos/generations \
  -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{"prompt":"一只橘猫在窗台上打哈欠","size":"16:9","resolution":"720P","duration":5}'
# → {"id":"video_50079dcb…","object":"video","status":"queued","upstream_id":"tsk_50079dcb…"}

# 轮询
curl -s localhost:8400/v1/videos/video_50079dcb… -H 'Authorization: Bearer <key>'
# → {"status":"in_progress"} → {"status":"completed","progress":100,
#     "url":"https://cdn.qwenlm.ai/…/t2v/….mp4?key=…",
#     "gateway_url":"/v1/videos/video_50079dcb…/content"}

# 下载成片（推荐走网关代理，见下）
curl -s localhost:8400/v1/videos/video_50079dcb…/content -H 'Authorization: Bearer <key>' -o out.mp4
```

> 🔴 **上游 CDN 有防盗链**：拿 `url` 里的裸链接直接下会得到 404（226 字节 HTML）。
> 所以响应里额外给 `gateway_url`，网关替你带 `Referer` 取回；直接用 `url` 的话，
> 客户端必须自己加 `Referer: https://chat.qwen.ai/`。

档位（上游按 `(resolution, duration)` 独立计价）：720P 5s=40 积分 / 10s=80 / 15s=120；
1080P 5s=70（走付费积分）。

> ⚠️ 任务 → 账号的映射只在内存保留 2 小时（查询必须用提交它的那个账号的凭据）。
> 进程重启后旧 `video_` id 会 404。

### `POST /v1/files` + 文件提问

```bash
# 1) 上传
curl -s localhost:8400/v1/files -H 'Authorization: Bearer <key>' \
  -F 'file=@report.pdf' -F 'purpose=assistants'
# → {"id":"file-cfa897…","object":"file","bytes":95,"filename":"report.pdf",…}

# 2) 提问（content part 引用 file_id）
curl -s localhost:8400/v1/chat/completions -H 'Authorization: Bearer <key>' \
  -H 'Content-Type: application/json' -d '{
    "model":"qwen3.7-plus",
    "messages":[{"role":"user","content":[
      {"type":"text","text":"这份文件讲了什么？"},
      {"type":"file","file":{"file_id":"file-cfa897…"}}
    ]}]
  }'
```

上游没有"文件库"：文件是**跟着消息走**的 —— 网关先在 `/v1/files` 完成 `getstsToken` 上传，
把上游的 `files[]` 条目暂存起来，提问时拼回 `messages[0].files`（`chat_type` 仍是 `t2t`）。

> ⚠️ 条目只在内存留 2 小时，且**与上传它的账号绑定**（file_id 是账号维度资源）。
> 进程重启后旧 `file-…` id 会 400。

### `GET /v1/models`

返回 7 个实测模型（上游拉取 + 10 分钟 TTL；拉不到就用内置清单，**绝不 5xx**）。

### `POST /v1/messages`（Anthropic）

Claude Code / Cline 这类客户端把 Base URL 指向本服务即可。按 Anthropic 事件序列回写
（`message_start` → `content_block_delta` → `message_delta` → `message_stop`）。

> 不做 `thinking` block：Anthropic 的 thinking 需要签名，网关不伪造签名。

### `POST /v1/responses`

供 Codex 之类只认 Responses 的客户端接入。只实现文本输出；`function_call` 等工具项
**明确 400**（上游没有函数调用通道，不假装支持）。

### 运维面

| 端点 | 用途 |
|---|---|
| `GET /healthz` | 存活（不带凭据） |
| `GET /readyz` | 就绪（无可用账号时 503） |
| `GET /stats` | 池快照 + 请求统计 + 模型缓存状态 |

## 配置

`config.json`（完整字段说明见 [`config.example.json`](config.example.json)）：

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `:8400` | 监听地址 |
| `api_keys` | `[]` | 前门鉴权；空 = 不鉴权（只建议本机/内网） |
| `default_model` | `qwen3.7-plus` | 请求未给 model 时用 |
| `default_thinking` | `auto` | 默认思考档 |
| `expose_reasoning` | `true` | 是否透出 `reasoning_content` |
| `new_chat_per_request` | `true` | 每请求新建会话（无状态） |
| `upstream.version` | `0.3.26` | **必须与当前前端版本自洽**，见 `docs/UPSTREAM.md` §2.2 |
| `upstream.bx_ua` | 空 = 自动生成 | `bx-ua`/`bx-umidtoken` 的值。**留空也会发**（自动生成 `load_failed` 形态）；🔴 缺这两个头时 `completions` 必吃 `RGV587` 风控 |
| `pool.max_rotate` | `3` | 单请求最多换号次数 |
| `pool.cooldown_soft_s` | `600` | 软冷却基数（指数退避） |
| `pool.cooldown_auth_s` | `1800` | 凭据失效冷却 |
| `pool.max_inflight_per_account` | `2` | 单号并发上限 |

环境变量（覆盖配置文件，前缀 `QWEN2API_`）：
`LISTEN` · `API_KEYS` · `ACCOUNTS`（逗号/空格分隔，**整体替换**配置里的列表）· `DEFAULT_MODEL` ·
`DEFAULT_THINKING` · `STATE_FILE` · `BASE_URL` · `VERSION` · `USER_AGENT` · `TIMEOUT_S`。

## 代码地图

| 我想… | 看这里 |
|---|---|
| 改上游请求头 / 请求体 / SSE 判读 | `internal/upstream/{headers,payload,sse}.go` |
| 改多账号怎么轮换、冷却、计数 | `internal/pool/pool.go` |
| 改 OpenAI 前门（拍平/流式/错误映射） | `internal/server/{chat,prompt}.go` |
| 改 Anthropic / Responses 前门 | `internal/server/{messages,responses}.go` |
| 改图片/视频前门 | `internal/server/{images,videos}.go` |
| 改 t2i/t2v 的请求体（chat_type / biz_data） | `internal/upstream/payload.go`（`ChatSpec`） |
| 改模型清单与缓存 | `internal/server/models.go` |
| 改控制台 | `internal/panel/index.html` |
| 上游到底长什么样（真源） | [`docs/UPSTREAM.md`](docs/UPSTREAM.md) |

```
cmd/server/          入口：加载配置 → 建池 → 起服务 → 优雅退出
internal/config/     JSON 配置 + 环境变量覆盖 + 校验
internal/upstream/   上游契约唯一实现处（头/请求体/SSE/错误分类）
internal/pool/       账号池：选号、在途限流、冷却、状态落盘
internal/server/     四类前门 + 运维面
internal/panel/      内嵌单页控制台
internal/reqlog/     请求流水（环形缓冲 + stdout 一行）
```

## 真实链路

| 步骤 | 结果 |
|---|---|
| 建会话 `/api/v2/chats/new` | ✅ 200，拿到 `data.id` |
| 流式对话（含深度思考） | ✅ `response.created` → `thinking_summary` → `answer` → `finished`，7 帧完整收流 |
| 思考摘要 diff | ✅ 累加数组按帧 diff，不重复吐字 |
| 真实 usage | ✅ `input/output/total` + `reasoning_tokens` |
| 模型清单 | ✅ 7 个 t2t 模型 |
| 建会话 `/api/v2/chats/new`（网关→上游） | ✅ 200 |
| 非流式对话（网关→上游） | ✅ `content="收到"` + 真实 `reasoning_content` + `usage` 2444/24 |
| 流式对话（网关→上游） | ✅ role → reasoning → content → finish → usage(含 `reasoning_tokens:239`) → `[DONE]` |
| 多轮 + system 拍平 | ✅ system 约束生效（每句「喵」结尾）、历史生效（记住"小明"）、`thinking:fast` 零思考 |
| `/v1/messages`（Anthropic） | ✅ `content=[{type:text,text:"你好呀"}]` |
| `/v1/responses`（Responses） | ✅ `output[0].content[0].text="你好呀"` |
| 控制台 | ✅ 账号/流水/试跑正常渲染 |
| **文生图** `/v1/images/generations` | ✅ 200，83.8s；`url` 模式返回 `cdn.qwenlm.ai/.../t2i/….png?key=<JWT>` |
| **图生图** `image` = data URI | ✅ 50.4s；产物与原图同构图、按提示词变水彩（人工核对） |
| **图生图 · 双参考图** | ✅ 136.7s / 2048²；"左红右蓝"精确执行（两张图都被消费，目视核对） |
| **尺寸矩阵** 1:1 / 16:9 / 9:16 / auto | ✅ 2048² / 2752×1536 / 1536×2752 / 2048² —— **比例真的生效** |
| **`n=2`** | ✅ 2 张，165.7s（串行两次上游调用，约 83s/张） |
| **额度耗尽** | ✅ 429 + `insufficient_quota` + 上游原话（识别 `extra.update_member`） |
| **每日额度（免费号）** | ✅ 全程压测：**9 张**后耗尽；中途 2 次「静默挂起」但自愈（非额度） |
| **挂起保护** | ✅ 图片硬超时 180s；文案区分"额度将尽 / 临时故障"（实测命中） |
| **本地每日配额** | ✅ 限额设 1 实测：第 1 张 69.9s 出图，第 2 张 **0.003s 直接 429**（未打上游） |
| **图生图 b64** `response_format:"b64_json"` | ✅ 53.9s，9.95MB base64 → 7.46MB PNG（`89 50 4E 47` 校验） |
| **`/v1/images/edits`**（multipart） | ✅ 51.7s，`-F image=@ref.png` 直接出图 |
| **文件解析** `/v1/files` + chat | ✅ 上传 95B txt → 提问 14.8s，原样答出文件内关键词 |
| **联网搜索** | ✅ `thinking:auto` + 明确要求，82s 返回当日科技新闻（带来源与 `[[40]]` 引用） |
| **文生图 b64** `response_format:"b64_json"` | ✅ 71.4s，7.5MB base64（含 CDN 就绪重试，见下） |
| **文生视频** `/v1/videos/generations` | ✅ 2.7s 拿到 `tsk_…` |
| **视频轮询 + 产物** | ✅ 约 15 分钟 `queued → in_progress → completed`，产物 `cdn.qwenlm.ai/.../t2v/….mp4`，1.7MB 可下 |

## 视频按积分计费（不是按个数）

上游视频是**积分制**，不同档位差 4 倍。价格来自用量页的真实扣费记录（2026-10-29）：

| 档位 | 积分 | 依据 |
|---|---|---|
| 720P 5s | **30** | 实测 `视频生成 - HappyHorse 1.1 - 720P - 5s  -30` |
| 1080P 15s | **120** | 实测 `视频生成 - HappyHorse 1.1 - 1080P - 15s  -120` |
| 720P 10s / 15s | 60 / 90 | 按秒线性推算（15s=5s×3 已被 1080P 档验证） |
| 1080P 5s / 10s | 40 / 80 | 同上 |

普号每天送 **40 积分**（用户确认 + 用量页 `已授予每日免费积分 +40`）。所以：

```
720P 5s  (30 分)  → 一天 1 个，剩 10
1080P 5s (40 分)  → 刚好 1 个
1080P 15s(120 分) → 需要攒 3 天
```

网关按**剩余预算是否够这一次**来选号，不是按"个数"——否则 40 分预算放行一个 120 分的请求，会直接透支 3 倍。

```json
"pool": { "max_video_credits_per_day": 40 },
"video": {
  "pricing": {
    "720P":  { "5": 30, "10": 60, "15": 90 },
    "1080P": { "5": 40, "10": 80, "15": 120 }
  }
}
```

会员号（如 Go 套餐：270 订阅 + 40/天）把 `max_video_credits_per_day` 调高即可。

## 每日出图配额（本地限制，默认 6 张/号）

上游免费号实测每日约 9 张，但**打到硬限时上游不报错** —— 只发心跳、永不产出，
要等 180s 才知道失败。所以网关自己先设一道闸：

```json
"pool": { "max_image_per_day": 6 }
```

- 只统计**成功出图**（实测挂起不扣上游额度，计进去会白占名额）；
- **按自然日重置**（上游口径是「今日」，不是滚动 24h），跨天自动清零；
- 配额**跨重启保留**（写进 `state_file`）——否则重启等于重置额度，会把上游打穿；
- 用尽后立刻返回 `429 insufficient_quota`，**不打上游**，并说明是本地限额；
- 面板「今日出图」列直接看 `已用/上限`。

## 风控与稳定性（必读）

上游对 `POST /api/v2/chat/completions` 有**行为指纹判定**（x5sec / RGV587）。实测（2026-10-28）：

| 现象 | 结论 |
|---|---|
| 请求头不齐（尤其漏 `X-Accel-Buffering: no`） | 必被拦：`RGV587_ERROR::SM::哎哟喂,被挤爆啦` + `punish?action=captcha` |
| `chats/new` 缺头 | **不拦**（阈值更低）⇒ 「建会话成功」不能证明头齐了 |
| 换 Chrome TLS 指纹（curl_cffi） | 缺头时**仍被拦** ⇒ 不是纯 TLS 层问题 |
| 等待 15 分钟 / 换账号 | **无效** ⇒ 不是限流，也不是账号封禁，是请求特征判定 |

**结论**：把页面在 completions 上显式设置的 8 个头 + 2 个 bx 头补齐后，Go 原生 TLS 栈
即可稳定通过（本仓已内置，`internal/upstream/headers.go`）。若仍被拦，先核对
`docs/UPSTREAM.md` §2.2 的头清单，而不是去换 IP 或降频。

## 诚实边界

- **`mask` 不转发**：上游没有 inpaint 语义。传了 `mask`（且带 `image`）仍回 200，但**不会**真的做局部重绘——
  这是 image-adapter 也维持的现状，宁可文档标注也不伪造能力。
- **TTS / 实时语音：明确不做**（有取证，不是偷懒）。`GET /api/v2/tts/config` 只返回**音色目录 +
  试听 mp3**（30 语言 / 50+54 音色），**没有合成端点**；「语音通话」进去后 UI 正常但
  fetch / WebSocket / getUserMedia 三个 hook 全 0 命中，CDP 40 条请求里也没有任何 ws/audio ——
  通道在 HTTP 层之外（WebRTC）。要支持它得先实现 WebRTC 客户端 + Opus 编解码，
  那就不是网关而是另一个客户端了。详见 `docs/UPSTREAM.md` §4.9。
- **MCP 工具（code interpreter / amap 等）**：模型自决调用，走的是工具调用帧。
  本仓已能解析 `image_gen` 系；其它 phase 会**记日志**（不再静默丢弃），但尚未翻译成结构化产物。
- **视频任务映射不持久化**：只在内存留 2h，重启后旧 id 404（任务在上游，重查要新 id）。
- **工具调用未实现**：上游网页端没有函数调用通道，网关不伪造 `tool_calls`。
- **无排队**：容量不足直接 429（这是同步链路，排队只会让客户端以为卡死）。
- **多轮靠拍平**：网关无状态、每请求新建会话，历史被拼进单条 prompt。省 token 的
  「复用 chat_id + parent 链」是后续优化项，当前不做（串味风险 > 那一次上游调用）。
- **token 寿命未知**：靠 401 被动失效 + 冷却兜底，不猜时长。
- **`/v1/messages/count_tokens` 是估算**（CJK 1:1、ASCII 4:1），不假装精确。

## 发版

push `main` → 测试 → 自增 patch（`v0.0.x`）→ 构建多架构镜像（amd64 + arm64）→
推 GHCR（版本 / `sha-` / `latest` 三 tag）→ 打 annotated tag → 建 Release。
逃生阀：commit message 含 `[skip release]`；人工版本号走 `workflow_dispatch`。

```bash
docker pull ghcr.io/yuanjie-ai/qwen2api:latest
```

## 致谢

- 上游契约的**第一版**来自 [`rsfree/qwen`](https://github.com/rsfree/qwen)（2026-09-24）——
  本仓在其基础上按 2026-10-28 的实测重新校准，并保留了它「降级说明只进服务端日志」
  「诚实边界写进文档」的做法。
- 工程结构（池 / 冷却 / 面板 / 发版流水线）参照 `workbuddy2api-panel`。
