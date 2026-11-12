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
| 🤖 **Agent 模式** | `qwen3.8-max-agent` / `qwen3.8-flash-agent` 走 `chat_type=agent_mode`：**服务端自主规划并执行工具**（bash / write_file / ask_user），一次请求内跑完 `think → tool_call → think → answer` 整个循环。实测一句「写 hello.py 并运行」= 4 轮工具、**自主纠错**（`cd /home/user` 失败后自己 `pwd`+`find` 找到真实沙箱路径）、11 秒出结果。工具轨迹记进请求流水（`tools` 字段）。⚠️ 需要澄清时上游抛 `ask_user` 交互工具，单轮 API 无法回答；产物文件不随响应下发（上游只给文件名列表） |
| ⚡ **流式 + 非流式** | 出站恒 `stream:true`，非流式由本地聚合（只有一条上游分支，行为可预测）；`stream_options.include_usage` 支持 |
| 🤝 **三协议前门** | `POST /v1/chat/completions`（OpenAI）· `POST /v1/messages`（Anthropic Messages，Claude Code 可接）· `POST /v1/responses`（Responses 形态） |
| 🖼️ **图片输入（多模态）** | chat 的 content 段支持图片：URL / data URI / 裸 base64，`image_url` / `input_image` / Anthropic `image` 三种写法全收；走 `getstsToken` → OSS 上传 → `files[]`（`file_class=vision`），上传与提交**同账号** |
| 🎨 **图片生成/编辑** | `POST /v1/images/generations`（文生图 + **图生图**）与 `POST /v1/images/edits`（multipart 前门）。字段集对齐 image-adapter 规范；`image` 支持 URL / data URI / 裸 base64，**参考图自动走 `getstsToken` → OSS V4 签名 PUT 上传** |
| 🎬 **视频生成** | **火山方舟原生协议**：`POST/GET/DELETE /api/v3/contents/generations/tasks`。创建只回 `id`、六态、`content.video_url`、幂等、回调、任务落盘。支持 720P/1080P × 5/10/15s × 6 档比例、文生 / 首帧 / 首尾帧 / 多参考图 |
| 🖼️ **媒体产物** | 图片返回带签名的 CDN 直链（`cdn.qwenlm.ai`），可选 `b64_json` 内联；视频返回 `tsk_` 任务的产物 URL |
| 📄 **文件解析** | `POST /v1/files` 上传（PDF/txt/任意文档）→ 在 chat 的 content part 里引用 `file_id`，模型直读内容。**实测**：95 字节 txt 里埋「紫色大象 42」，提问后原样答出 |
| 🔍 **联网搜索** | 上游 `feature_config.auto_search` 默认开启；`thinking: auto` + 明确要求即可检索当日新闻并附来源链接 |
| ⚡ **会话预热** | 建会话（`chats/new`）实测 **2.9~3.8 秒**。热点号提前各建一个空会话（`warm_chats_per_account`，默认 1），**净省约 1.2 秒/次（-18%）**；覆盖范围跟着流量动态缩放，用掉即补。**会话是账号维度资源**（跨号提交会 `CHAT_NOT_FOUND`），所以按号建、按号用 |
| 🔁 **失败换号** | 只对「可证明未受理」的失败换号：①语义明确的 401/额度/429/风控；②**快速失败**（≤10s 就挂的上游错误——连活都没干）。**慢失败**（拖了几十秒才报的 5xx、生图"零产物超时"）**不重试**，绝不赌重复扣费 —— 生图超时只把该号冷却 10 分钟，然后如实回 504（预算已 300s，再等一轮 = 让调用方挂 10 分钟） |
| 🧾 **真实 usage** | 输入/输出/思考 token 全部来自上游，不做本地估算（`count_tokens` 除外，那里明写是估算） |
| 🖥️ **Web 控制台** | 内嵌单页（零外部依赖）：账号池状态 · 请求流水 · 首字/耗时/token · **对话框调试（文本流式 / 图片 / 视频，逐轮可看请求体与响应）** · 一键重置账号 |
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

#### 图片输入（多模态）

`content` 里放图片段即可，三种写法都收：

```bash
curl -s localhost:8400/v1/chat/completions \
  -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
      {"type":"text","text":"这张图是什么颜色？只回答颜色名"},
      {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo…"}}]}]}'
```

| 写法 | 形态 | 出处 |
|---|---|---|
| `{"type":"image_url","image_url":{"url":"…"}}` | 官方 OpenAI chat；`image_url` 直接是字符串也收 | chat |
| `{"type":"input_image","image_url":"…"}` | Responses 形态 | `/v1/responses` |
| `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}` | Anthropic；`source.type=url` 亦可 | `/v1/messages` |

引用内容三条都收：**URL / data URI / 裸 base64**（与 `/v1/images` 的 `image` 字段同一条解链；
裸 base64 按魔数嗅探，不信声明）。单次最多 20 张，超了 400 并指向 `param=image`。

链路（`chat_type` 仍是 **`t2t`**，条目带 `file_class=vision`）：

| 引用形态 | 网关动作 |
|---|---|
| `http(s)://…` | **原样**写进 `files[].url`，上游自己来拉。不下载、不上传 |
| `data:image/…;base64,…` / 裸 base64 | 解码后存进内存 `BlobStore`，改给 `https://<blob_base_url>/v1/blobs/<32hex>.png` |

`/v1/blobs/<id>` 是**免鉴权**的只读端点（上游取图不带我们的 Key），安全性靠
128 位随机 ID + 2 小时 TTL + 只读。

> 🔴 **为什么不走 `getstsToken` 上传了**（2026-10-30 实测）：该端点已被阿里云 WAF
> 加了人机验证（滑块页，HTTP 200 包 HTML）。Bearer / Cookie / 带 bx-ua / 不带 /
> 预热 acw_tc+x-ap / 14 个不同出口 / 直连 —— 9 种组合全部被挑战；而同一出口的
> `/api/v2/models/` 正常。**但 `files[].url` 给外链完全够用**：实测模型能准确答出
> 纯红图的颜色、读出带字图里的 `HELLO42`。详见 `docs/UPSTREAM.md` §6.1。
>
> 🔴 任何一段图片引用解析失败都 **400**，绝不"跳过这张图继续聊"——
> 那会让模型看不见图却照常作答，调用方以为多模态生效了。
>
> 📌 `blob_base_url` 必须配成公网域名（上游是从它自己的机房来拉图的，内网地址拉不到）。

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

> `url` 就是**上游 cdn 原链**，原样透传，不做改写、不落盘、不转存。
> ⚠️ 该链有 UA + Referer 双重防盗链（见 `docs/UPSTREAM.md` §4.5.6）：浏览器地址栏直输能开，
> 网页 `<img>` / 服务端 SDK 拉取会 403/404。需要绕过时用同一网关的
> `GET /v1/media/<原 path>` 代理（免鉴权、实时透传），见下方运维面表。

| 字段 | 说明 |
|---|---|
| `prompt` | 无 `image` 时必填 |
| `image` | `str` 或 `str[]`（URL / data URI / 裸 base64），最多 20 张。非空即走**图生图**：以 `chat_type=image_edit` 提交，参考图同样走 **URL 直通 / 本地 blob**（不再过被 WAF 挡住的 STS）。实测：白底黑字 `HELLO42` + 「背景改蓝、保留文字」⇒ 产物 2752×1376 蓝底黑字（宽高比与原图一致，是真编辑不是重画） |
| `mask` | 没有 `image` 时传了就是 400（面门收下但不转发——上游无 mask 语义，绝不假装 inpaint） |
| `n` | 非法值归一为 1；上限 4（每张都是一次独立上游调用 + 一次配额） |
| `size` | `1024x1024` / `1:1` / `auto` 都收，内部映射成上游 5 档比例 |
| `response_format` | `url`（默认）/ `b64_json`；不认识的值视为未指定，不报错 |
| `quality` / `style` / `user` | 收下但不影响上游（qwen 没有这些概念，不假装支持） |

> 🔴 `prompt` **原样发给上游**，不要自己加「请画一张图：」之类的包装。t2i 是显式类型，
> 上游按 `chat_type` 分派；被改写的 prompt 会让它静默空转（实测 200s 无内容）。
>
> ⚠️ **产物链接的坑**（实测，本仓已兜住）：
> 1. **刚生成的那几秒 CDN 还没转存完** —— 立刻下会得到 404 + 226 字节 HTML「死链」。
>    `b64_json` 模式带退避重试；`/v1/media` 代理对 404 也重试一次。
> 2. **原链有两道防盗链** —— `curl/*` 这类 UA 被 WAF 挡成 404（返回体与「没转存」
>    一模一样，极容易误诊）；带外站 Referer 则 403。`url` 原样返回，所以**自己下载要带
>    浏览器 UA**；不想操心就用 `/v1/media` 代理。
> 3. 判真假看 **magic bytes**，不看 `Content-Type`（真图可能回 `application/octet-stream`）；
>    `/v1/media` 在 `Content-Type` 不是 `image/*` 时按扩展名兜底。

### `POST /api/v3/contents/generations/tasks`（火山方舟原生协议）

视频出口**逐字对齐火山方舟数据面**：客户端把 base_url 指到
`https://qwen2api.1task.cn/api/v3`，方舟 SDK / Seedance 协议客户端可直接对接。

| surface | 方法 | 回什么 |
|---|---|---|
| 创建任务 | `POST /contents/generations/tasks` | `{"id":"cgt-…"}`（**只有 id**，受理 ≠ 成功） |
| 查询任务 | `GET /contents/generations/tasks/{id}` | 完整任务对象 |
| 查询列表 | `GET /contents/generations/tasks` | `{items[], total}` |
| 取消 / 删除 | `DELETE /contents/generations/tasks/{id}` | `200 + {"Result":{}}` |
| 取成片 | `GET /contents/generations/tasks/{id}/content` | mp4 字节（**网关扩展**，方舟无此端点） |

```bash
B=https://qwen2api.1task.cn/api/v3/contents/generations/tasks

# 提交（content[] 是唯一入参载体；只回 id）
curl -s $B -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{"model":"happyhorse11",
       "content":[{"type":"text","text":"一只橘猫在窗台上打哈欠"}],
       "ratio":"16:9","resolution":"720p","duration":5,"generate_audio":true}'
# → {"id":"cgt-20261108194012-a1b2c3d4"}

# 轮询（?refresh=1 跳过 2 秒节流）
curl -s "$B/cgt-20261108194012-a1b2c3d4?refresh=1" -H 'Authorization: Bearer <key>'
# → {"status":"running","content":{"video_url":null},"error":null,
#    "usage":{"completion_tokens":30,"total_tokens":30,"credits":30},…}

# 列表（本地快照）/ 删除（仅终态）
curl -s "$B?filter.status=succeeded&page_size=20" -H 'Authorization: Bearer <key>'
curl -s -X DELETE "$B/cgt-…" -H 'Authorization: Bearer <key>'

# 取成片（推荐：网关带浏览器 UA 代取，绕开 CDN 的 UA 拦截）
curl -s "$B/cgt-…/content" -H 'Authorization: Bearer <key>' -o out.mp4
```

列表参数按方舟原生：`filter.model` / `filter.status` / `filter.task_ids`（重复参数名）
/ `page_num` / `page_size`（均 `[1,500]`，默认 1 / 20）。`total` 是**过滤后的总数**，不是本页条数。

六态：`queued` / `running` / `succeeded` / `failed` / `expired` / `cancelled`。
认不出的上游状态**收敛为 `running`（非终态）** 并带回 `status_raw`。

| 纪律 | 为什么 |
|---|---|
| 创建回包**只有 `id`** | 受理 ≠ 成功。往里塞 status 就是承诺一个客户端不该依赖的形状 |
| `content.video_url` 仅 `succeeded` 有值 | 未成功就交地址等于撒谎 |
| 参数双载体二选一 | body 字段与 prompt 后缀（`--rs/--rt/--dur`）同时出现即 400 |
| 幂等 | 同一份意图（指纹相同、未终态、1 小时内）重复提交返回同一个任务，**不重复计费** |
| 任务映射落盘 | `data/videos.json`，保留 8 天。上游任务活 7 天、产物 24h，只存内存等于重启即丢 id |
| 回调 | `callback_url` 支持；推送体与查询响应由**同一个序列化器**产出，按 `(id,status)` 幂等 |

#### 🔴 与方舟原生的已知偏差（做不到的事，一律显式 400，不静默降级）

| 方舟行为 | 本网关 | 原因 |
|---|---|---|
| `usage.completion_tokens` 是 token 数 | **等于本次扣的积分** | Qwen 不统计 token，只扣积分。另给 `usage.credits` 明文标注 |
| `DELETE` 对 `queued` → `cancelled` | **400 `InvalidParameter.OperationDenied`** | 上游没有取消端点。假装取消 = 客户端以为停了、实际还在跑并计费 |
| `asset://` 素材 ID | 400 | 上游没有素材库 |
| `frames` | 400 | 上游只认秒级 duration |
| `duration=-1`（模型自选） | 400 | 上游不支持；时长直接决定积分 |
| `duration` 4~30 任意值 | 只 5 / 10 / 15 | 上游档位 |
| `resolution` 480p / 4k | 只 720p / 1080p | 上游档位 |
| `ratio` 21:9 | 400 | 上游 6 档不含 21:9 |
| `watermark` / `seed` / `camera_fixed` / `return_last_frame` / `output_format` / `draft` / `service_tier` / `priority` / `tools` | 接受但不生效，记 `X-Qwen2API-Warnings` 响应头 | 上游无对应参数 |
| `filter.service_tier` | 忽略（不过滤） | 上游无服务等级概念 |
| 参考视频 / 参考音频 | 400 | 上游 `attachments` 只接受 `{type:"image"}` |

#### 模型名与别名

`model` 是**路由名**，不是上游模型 ID。解析顺序：

1. **内置内核名**：`happyhorse11` / `happyhorse` / `wan27`
   （也认 `HappyHorse 1.1`、`wan-2.7` 这类写法，忽略大小写与分隔符）
2. **配置别名**：`config.video.aliases` 里的 `客户名 → 内核`
3. 都不是 ⇒ 当 chat 模型名，内核用 `config.video.root_model`

无论走哪条，响应里的 `model` **回显客户端原名** —— 客户端按回显名做路由时不会找不到模型。

```json
"video": {
  "aliases": {
    "doubao-seedance-2-0-pro-260628": "happyhorse11",
    "doubao-seedance-1-0-lite-t2v-250428": "wan27"
  }
}
```

> ⚠️ 别名只是**路由标签**：产物由本网关的上游生成，**与 doubao 模型无关**。
> 配 `doubao-seedance-*` 只是给方舟客户端一个熟悉的名字，
> 不要拿它对照字节的报价单 —— `/v1/models` 里每个别名条目的 notes 也写明了这句。

`/v1/models` 同时列出内核条目与别名条目；`params.modes` 给出该内核支持的场景
（`t2v` / `first_frame` / `first_last` / `omni_ref`），`params.sizes` 是实测 6 档比例。

#### 能力矩阵按内核分档（🔴 换内核就换能力）

取证：上游前端 bundle `qwen-chat-fe/0.3.38/js/main.js`（2026-11-08 只读，零消耗）。
原文常量 `ss = {"happyhorse-1.1":["standard","multi-reference"], "wan-2.7":["standard","first-last-frame"]}`、
`ms = {standard:1, "multi-reference":9, "first-last-frame":2}`。

| 内核 | 文生 | 多参考图 | 首帧 / 首尾帧 |
|---|---|---|---|
| `happyhorse11`（默认） | ✅ | ✅ 最多 9 张 | ❌ |
| `happyhorse`（1.0） | ✅ | ✅ 最多 9 张 | ❌ |
| `wan27`（wan-2.7） | ✅ | ❌ | ✅ 2 张（首帧 + 尾帧） |

- 参考图用 `role: "reference_image"`；首尾帧必须两张都显式给 `role`（`first_frame` + `last_frame`）。
- 首帧/尾帧与参考图**混用即 400 `InvalidParameter.TaskTypeConstraint`**。
- 比例域是 **6 档**：`16:9` / `4:3` / `1:1` / `3:4` / `9:16` / `adaptive`（上游叫 `original`，发上去是 `origin`）。

> ✅ 参考素材已实测打通（2026-11-08）：`files[]` 上传后的 `id`（`img-<uuid>`）可直接当
> `params.attachments[].materialId`。验证用**可机检的纯色图**：产物路径落在 `.../i2v/...`
> （文生是 `t2v`），首帧 R=252/B=0、末帧 R=0/B=244、第 75 帧 R=235/B=11（正在过渡）。
>
> 🔴 首尾帧的**唯一标识**是 `extra.meta.videoGenerationMode = "first-last-frame"`，
> 不是 `genMode` —— 首尾帧与纯文生的 `genMode` **都是 `vid_gen`**。
> 漏了它，上游会按纯文生处理：只吃第一张图，第二张直接丢，而接口一路 200、积分照扣。
>
> ⚠️ `ratio` 按场景分两种行为（2026-11-08 四场景实测）：
>
> | 场景 | 请求 16:9 的实际产物 | ratio |
> |---|---|---|
> | 纯文生 `t2v` | 1280×720 | ✅ 生效 |
> | 多参考图 `multi-reference` | 1280×720 | ✅ 生效 |
> | 单图首帧 `standard` | 960×960 | ❌ 被输入图覆盖 |
> | 首尾帧 `first-last-frame` | 960×960 | ❌ 被输入图覆盖 |
>
> 实测耗时（5 秒片）：t2v **约 7~8 分钟**、单图首帧约 4.5 分钟、首尾帧 47 秒、多参考图 60 秒。
> 客户端轮询预算别按 i2v 的一分钟设计。
>
> 紧急退回纯文生：`QWEN2API_VIDEO_MATERIAL_DISABLE=1`（不必回滚版本）。

> 🔴 **`/v1/videos` 与 `/v1/videos/generations` 已下线，返回 410 `EndpointRetired`**。
> 视频出口只保留方舟路径 —— 两套形状并存必然出现「两边字段对不上」。

>
> 紧急退回纯文生：`QWEN2API_VIDEO_MATERIAL_DISABLE=1`（不必回滚版本）。

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

返回 **11 个条目 = 7 个文本模型 + 2 个 Agent 模型 + 1 个图片模型 + 1 个视频模型**。
文本模型来自上游（10 分钟 TTL；拉不到就用内置清单，**绝不 5xx**），Agent 与生成模型由网关补上。

```json
[
  {"id": "qwen3.7-plus",   "media": "text",  "thinking": true, "vision": true},
  {"id": "qwen3.8-max-agent", "media": "text", "thinking": false, "vision": true},
  {"id": "qwen-image-3.0", "media": "image", "endpoint": "/v1/images/generations",
   "params": {"sizes": ["1:1","4:3","3:4","16:9","9:16"], "max_n": 4}},
  {"id": "happyhorse11",   "media": "video", "endpoint": "/api/v3/contents/generations/tasks",
   "params": {"resolutions": ["720P","1080P"], "durations": [5,10,15],
              "credits": {"720P": {"5": 30, "10": 60, "15": 90}}}}
]
```

### Agent 模型（`qwen3.8-max-agent` / `qwen3.8-flash-agent`）

Agent 模式的上游**模型面是独立清单**（`GET /api/v2/models/agent_mode`，实测只有
`qwen3.8-max` 与 `qwen3.8-flash`），与普通 chat 的 7 个模型不重叠。所以本网关用**别名**
暴露，避免同一个 id 的 `chat_type` 变得不可预测：

| 你在 `model` 里写 | 上游 `model` | `chat_type` / `sub_chat_type` |
|---|---|---|
| `qwen3.8-max-agent` | `qwen3.8-max` | `agent_mode` / `lite` |
| `qwen3.8-flash-agent` | `qwen3.8-flash` | `agent_mode` / `lite` |
| 其他任何 chat 模型 | 原样 | `t2t` / `t2t` |

- `thinking` 参数**对 Agent 无效**：上游 Agent 没有「自动/思考/快速」档位，固定 Fast
  （`feature_config` 只有 5 个键，且不带 `auto_search`）；
- 思考走 `reasoning_content`，但 Agent 给的是**思考原文增量**（普通模式给的是摘要累加数组）；
- `usage` 上游放在 `extra.model_service_info`（顶层恒空），网关已兜底读取，
  且取 `task_usage`（整个任务累计，含每一轮工具）；
- 🤝 **对话粘性（默认开）**：同一个对话的多轮会**复用上游会话**，吃满 prompt cache ——
  实测同一 `chat_id` 内那 ~14k 的 agent 系统提示只有首轮全价，之后 13k+ 命中缓存。
  对话键三级优先：`X-Dialog-Id` 头 → `user` 字段 → **`hash(model + 全部 system + 首条用户消息)`**
  （零改造兜底；锚点取全文而非前缀，system 参与是为了把不同应用分开）。
  配置项 `dialog_sticky`（默认 true），关掉即回到"每请求新建会话"。
- 🔴 **粘性只是省钱，不承担上下文**：历史仍由客户端每轮发全量 `messages`、
    我们拍平进 prompt 保证。所以拿不到会话（并发/换号/重启）时一律**换新号开新会话**，
    绝不阻塞、绝不回 409 —— 代价只是一次 cache miss。
- ⚠️ 并发同一个对话：**自动降级为新号新会话**（同一 `parentId` 并发会让上游对话链分叉）。
    流水里 `dlg=hit` / `dlg=new` 就是这套机制的观测口径。
- 🔴 **上游 agent 的账号级并发槽只有 1 个**（实测 `user.too_many_concurrent`）：
    第二轮紧接着发会被拒。网关的处理是「等 1.2s 重试同号保 cache → 还满才换号」，
    不会把这条"稍等"直接变成 502。
- 📈 实测收益：同一对话第二轮 **11.2s → 2.4s**（`dlg=hit` + `chat=cached`）。
- 🛡️ **短首句不粘**：首条用户消息短于 8 个字符（"你好"/"1"/"继续"）时**不启用** hash 兜底 ——
  这类句子在真实流量里高度重复，撞键会把两段无关对话并成一段（答非所问 + 泄漏上下文）。
  要短句也粘就显式传 `X-Dialog-Id` 或 `user`，那两级不受长度限制。

🔴 **上游没有独立的图片/视频模型 id** —— 2026-10-30 实测 `GET /api/v2/models/` 只有
7 个 chat 模型；图片靠 `extra.meta.model`（`qwen-image-3.0`）选内核，视频靠
`biz_data.rootModel`（`happyhorse11`）选内核。所以清单里的这两个是**本网关的模型名**，
传它网关会翻译成上游那两个字段：

| 你在 `model` 里写 | 建会话用的 chat 模型 | 生成内核 |
|---|---|---|
| 不填 / `qwen3.7-plus` 等文本模型 | 你填的那个 | 配置值（`image_model` / `root_model`） |
| `qwen-image-3.0`（或 `Qwen-Image 3.0`） | 默认文本模型 | `qwen-image-3.0` |
| `happyhorse11`（或 `HappyHorse 1.1`） | 默认文本模型 | `happyhorse11` |

- 名字比较**忽略大小写、空格与分隔符**：上游 UI 显示的 `Qwen-Image 3.0` /
  `HappyHorse 1.1` 可以直接抄过来用；
- 拿生成模型调 `/v1/chat/completions` 会 **400 并指路**（"请改调 /v1/images/generations"），
  而不是把模型名甩给上游换回一个看不懂的 `Bad_Request`；
- `endpoint` / `params` 是**加性扩展字段**，只取 `id/object/created/owned_by` 的客户端
  照样能用。

### `POST /v1/messages`（Anthropic）

Claude Code / Cline 这类客户端把 Base URL 指向本服务即可。按 Anthropic 事件序列回写
（`message_start` → `content_block_delta` → `message_delta` → `message_stop`）。

> 不做 `thinking` block：Anthropic 的 thinking 需要签名，网关不伪造签名。

### `POST /v1/responses`

供 Codex 之类只认 Responses 的客户端接入。只实现文本输出；`function_call` 等工具项
**明确 400**（上游没有函数调用通道，不假装支持）。

### 错误语义（客户端看到什么）

| 场景 | HTTP | `error.type` | `error.code` | `message` |
|---|---|---|---|---|
| API Key 缺失 / 无效 | **401** | `authentication_error` | `AuthenticationError` | `无效的 API Key` |
| 上游 180s 只发心跳、无产物 | **504** | `upstream_timeout` | — | `upstream timeout, please retry later` |
| 上游 5xx / 传输失败 / 形态不符 | **502** | `upstream_error` | — | `upstream error, please retry later` |
| 无可用账号（全冷却/失效） | **503** | `service_unavailable` | — | `no available upstream account, please retry later` |
| 上游凭据坏了（网关侧） | 502 | `upstream_error` | — | `upstream authentication failed` |
| 额度/限流 | 429 | `insufficient_quota` / `rate_limit_error` | — | 具体到"哪个额度、差多少"（中文） |
| 参数写错 | 4xx | `invalid_request_error` | 见下 | 具体到字段（中文） |

> 📌 `error.code` 用**方舟词表**，与 `error.type` 分工不同：
> `type` 决定「要不要重试」，`code` 决定「要不要改参数」。
>
> | `code` | 场景 |
> |---|---|
> | `AuthenticationError` | 401 —— **全端点**（鉴权是横切错误，`wrap` 统一产生） |
> | `MissingParameter` | 视频：缺 `content` / `model` |
> | `InvalidParameter` | 视频：值格式不对 |
> | `InvalidParameter.UnsupportedParameter` | 视频：上游没有这个能力（首尾帧换内核、比例、时长、分辨率） |
> | `InvalidParameter.TaskTypeConstraint` | 视频：首帧/尾帧与参考图混用 |
> | `InvalidParameter.OperationDenied` | 视频：非终态 DELETE（上游无取消端点） |
> | `ResourceNotFound` | 视频：未知任务 id |
>
> chat / images / messages 的参数错目前只给 `type`（OpenAI 的 `code` 本就是可空字段），
> 只有 401 是双方共用的 —— 它必须带码，否则方舟客户端按 `code` 分支时拿到空值。

**5xx 一律是短英文 + 稳定 type**（客户端/SDK 要的是机器可读，不是排查指南）；
"额度将尽 or 上游抖动 / 建议换号"这类判断只进**请求流水与容器日志**，
面板「日志」页照常看得到 —— 信息不丢，只是不外泄。429 与 4xx 保留具体中文，
因为那儿的信息（差多少积分、哪个字段超限）对调用方真的有用。

### 运维面

| 端点 | 用途 |
|---|---|
| `GET /healthz` | 存活（不带凭据） |
| `GET /readyz` | 就绪（无可用账号时 503） |
| `GET /stats` | 池快照 + 请求统计 + 模型缓存状态 |
| `GET /v1/media/output/...` | 图片直链**实时代理**（免鉴权、不落盘）。替调用方补上上游 CDN 要求的 UA/Referer，用于原链被 403/404 时绕行；`url` 字段默认不用它（原样返回上游直链），见 `docs/UPSTREAM.md` §4.5.6 |

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

## 管理面板

`/panel/`（**密钥登录**，见下；也支持 `?key=` 直接进）。五块视图，全部读真实接口，
**没有任何假数据或占位卡**：

| 视图 | 做什么 |
|---|---|
| **账号池** | 统计 + 搜索/状态筛选/排序/分页；单号「重登 / 重置 / 删除」；勾选后批量重置或删除 |
| **用量** | 累计请求、成功率、Token、平均耗时；请求流水（最近 80 条，含**账号与出口 IP**）+ 按模型/按账号分布 |
| **容量** | 每号今日剩余出图张数、余额折算「可做几个 720P 5s / 1080P 15s」、**每日赠送入账时刻**（悬停看预计下次），以及全池合计 |
| **模型与调试** | 模型清单（能力标记）+ 在线调试（文本流式 / 图片 / 视频；图片可垫**多张参考图**，最多 20 张、合计 16MB，按序号提交） |
| **配置** | **可编辑表单 + 热更新**（20 个字段，分基础/账号池/视频/上游；保存时写回 config.json 并**立即替换运行时快照**，除标「需重启」的项），另有生效配置只读摘要（服务/上游/配额/视频单价/账号清单来源） |

### 登录（密钥优先，与 `/v1` 的令牌分开）

面板能做导入/删除账号、能看到全部邮箱 —— 所以它有自己的凭据，默认就是**一把密钥**：

```json
"panel": {
  "key": "sk-panel-change-me",
  "session_hours": 720,
  "session_secret": "多副本部署时必须配同一个"
}
```

打开 `/panel/` 直接看到「输入密钥进入」：

- 填 `panel.key`（也可用环境变量 `QWEN2API_PANEL_KEY`）；**没配则认 `api_keys` 里任意一把**，
  单 Key 部署升级后不用改配置；
- 提交后服务端换一个 **HttpOnly + SameSite=Lax 的签名 cookie**（HMAC-SHA256），
  明文密钥**不落 localStorage**（旧版会存，等于把长期凭据交给 XSS）；
- 会话默认 **30 天**（`panel.session_hours` 可调）；签名密钥留空时**由凭据派生**，
  所以**重启/发版不掉线**（配了 `session_secret` 则以它为准，用于轮换）；
- `?key=…` 深链会先拿它换会话，再把参数从地址栏抹掉；
- 脚本 / CI 仍可直接带 `Authorization: Bearer <key>`、`x-api-key` 或 `?key=` 调 `/panel/api/*`；
- 🔴 **面板密钥只在面板生效，`/v1` 不认它** —— 面板能删账号，权限边界必须清晰；
- 连续 10 次失败 → 同 IP 锁 10 分钟（面板在公网，没这道闸等于开放爆破）。

还想用账密？配上 `password_hash`（`qwen2api -passwd '你的密码'`，省得手算迭代次数）或
`password` / `QWEN2API_PANEL_PASSWORD` 即可，登录框会多一个「改用账号密码登录」入口；
**两样都不配** = 面板不需要登录（只建议本机/内网）。

> 会话与用户名绑定：改用户名（或换 `session_secret`）即全部失效；
> `Secure` 标记按 `X-Forwarded-Proto` 自动加（nginx 终结 TLS 时必须靠它）。

### 导入账号

右上角「导入账号」，三种方式：

```
粘贴文本：每行「邮箱 密码 [账号名] [备注]」
         分隔符支持 空格 / 逗号 / 竖线 / 制表符，也接受 ---- 
导入文件：.txt 或 .json（内容会填进文本框，可先核对再导）
单个添加：表单
```

也支持直接粘一个 JSON 数组：

```json
[{"name":"main","email":"a@b.com","password":"pw","note":"Go 套餐"}]
```

默认**逐个登录校验**后才入池 —— 不校验等于往池里塞哑弹：错密码的号会被选号选中、
请求失败、然后冷却，表面看像上游抖动。校验失败的行会逐条给出原因（`邮箱或密码错误` /
`被上游风控拦截` / `登录超时`），成功的才入库。

### 账号清单存在哪（重要）

```
config.json     声明式基线（部署时只读挂载）
data/accounts.json   面板托管（导入/删除写这里；**存在即完全接管**）
```

不直接回写 `config.json` 有两个理由：部署时它是 `:ro` 挂载；而且让程序改用户的
声明式配置风险太大（一个格式 bug 能把 `pool`、`video` 一起写坏）。

「完全接管」而不是「合并」是刻意的 —— 合并会让「删掉的号重启后又回来了」。
想退回声明式配置：删掉 `accounts.json` 再重启。

> 文件权限 `0600`、原子写（同目录临时文件 + rename）。里面是明文密码，别入库：
> 部署目录的 `.gitignore` 已经排除它。

### 轮询与开销

前端 5s 拉一次 `/panel/api/overview`；余额刷新走 **2 分钟 TTL**，所以轮询不会把
上游积分域名打热。手动点「刷新余额」则无视 TTL。

### 会话预热（省掉请求开头的 2 秒）

`POST /api/v2/chats/new` 实测 **2.9~3.8 秒**（网关长连接下，见 `build_ms_avg`），
占一次对话的一半以上。所以网关会**提前给热点号各建一个空会话**：

| 项 | 说明 |
|---|---|
| 覆盖范围 | 与 access token 预热同源：`warmTarget`（按最近 10 分钟流量估算），单轮数量 `warmBatch` 一起缩放 |
| 只补 `t2t` | 对话占请求绝大多数；t2i / image_edit / t2v 一次就 50~90 秒，2.4s 占比可忽略 |
| 用掉即补 | 取出即删（会话提交后就有消息，复用会变成多轮），并立刻异步补一个 |
| 缓存上限 | `chat_pool_ttl_s`（默认 900s）—— 上游是否让空会话过期无实测依据，保守取与 access 同量级 |
| 关闭 | `warm_chats_per_account: 0` |

> 🔴 **会话是账号维度资源**：A 号建的 chat 让 B 号提交会得到 `CHAT_NOT_FOUND`（"此对话已删除"，
> 实测）。所以池子按 `(账号, 会话类型)` 分组，且只能预热"马上要用的号"（`pool.HotNames`）。
>
> 观测：`/stats` 的 `keepalive.chats = {per, ttl_s, ready, hit, miss, hit_pct, build_ms_avg}`；
> 流水里 `chat=cached`（命中）/ `chat=3087ms`（现建）。

#### 实测收益：净省约 1.2 秒（-18%），不是"省掉全部 3 秒"

同一批请求、只切换 `warm_chats_per_account` 做的对照（各 10~11 次，单轮对话）：

| | 总耗时均值 | 建会话 | 提交+读流 |
|---|---|---|---|
| 关闭预建（全部现建） | **6674ms** | 3221ms | 3453ms |
| 开启预建（命中） | **5497ms** | 0 | 5497ms |

⇒ **净省 1177ms（-18%）**。注意"提交+读流"在命中时反而多花约 2 秒 ——
上游对"几分钟前建好、一直空着的会话"要重新加载上下文，所以省下的 3.2s 里有 2s 又还了回去。
这也解释了为什么光看"建会话 3 秒"会高估收益：**只有净差才是真收益**。

### 配置热更新（面板改，立即生效）

面板「配置」页是一张**表单**，字段由后端 `config.Fields` 下发（**白名单**：只列 20 项
可安全修改的）。保存时：

1. 读原 `config.json`（保留 `// 注释键` 与所有未提交字段）
2. 按字段类型/范围校验（`pool.max_rotate` 只收 1~10，枚举只收列出的值，未知键直接拒绝）
3. `tmp + rename` 原子写回
4. **整体替换运行时快照** → 账号池限额/冷却/会话池参数当场生效，不用重启

生效方式：

| 类型 | 字段 | 生效 |
|---|---|---|
| 全部 pool 参数 | `max_rotate` / `cooldown_*` / `max_inflight_per_account` / `max_image_per_day` / `max_video_*` / `warm_chats_per_account` / `chat_pool_ttl_s` | **立即** |
| 模型与开关 | `default_model` / `default_thinking` / `expose_reasoning` / `image_model` / `blob_base_url` | **立即** |
| 生图预算 | `image_timeout_s`（单次尝试，默认 300s） | **立即** |
| 视频 | `root_model` / `resolution` / `duration` / `size` | **立即** |
| 出口探测 | `upstream.egress_ip_url` | **立即** |
| 上游超时 | `upstream.timeout_s` | 需重启（HTTP 客户端启动时建好） |

🔴 **不在白名单里的永远不能从面板改**：`listen` / `state_file` / `api_keys` / `panel.*` /
`upstream.base_url` / `accounts` —— 要么是启动期资源（热更会把整池账号抹掉），
要么写错就把服务锁死。这些只能改文件 + 重启，面板里会写明。

> 没给 `-config`（纯环境变量部署）时，表单只读，保存返回 400 并说明原因。
> 配置文件里的 `// 说明` 键会被原样保留（读进 `map[string]any` 再写回）。

**部署要求（否则面板保存会失败）**：

| 现象 | 原因 | 处置 |
|---|---|---|
| `read-only file system` | compose 里挂了 `./config.json:/app/config.json:ro` | 去掉 `:ro`（想保留只读就别用面板改配置） |
| `rename ... device or resource busy` | 单文件 bind mount 换不了 inode | 网关会自动降级成"备份 + 覆盖写"，日志会写一行 |

> 单文件挂载走覆盖写时**不是原子的**：写之前会先落一份 `config.json.bak`，
> 真写坏了就用它恢复（`cp config.json.bak config.json` 后重启）。目录挂载仍走 tmp+rename。

### 选号策略：优先"token 还活着"的号

池里 access token 的 TTL 只有 **15 分钟**，而 3000+ 个号不可能全都热着。所以选号
（`Pick` / `PickForImage` / `PickForVideo`）按这个顺序排：

1. **token 还活着**（剩余 > 2 分钟）的优先 —— 否则每个请求都要多一次 refresh
   或吃一次 401 再续期，白等几百毫秒；
2. 其次 `served` 最少（负载在池内摊平）；
3. 再其次在途最少、最久没用。

保活预热与选号**同源**：活着的号按"越快过期越先续"保温（不续它们就会在流量上
过期，续期就落到请求路径上）；活着的不够 `warmTarget` 时，用 `served` 升序的
过期号补齐 —— 那正是接下来要用的那批。全池都过期时排序不会卡死（优先 ≠ 过滤）。

## 出口代理（每个账号一条固定 IP）

所有账号从同一个 IP 登录/请求时，**一个号被判风控会连带整池**——上游是按出口 IP
做画像的。所以支持"账号 → 固定出口"的稳定映射（不是随机轮换，轮换反而更容易掉登录态）：

```json
"upstream": {
  "proxy": {
    "enabled": true,
    "url_template": "socks5h://poolclient-{account}:{password}@pool.livetest.cn:2090",
    "password": "按需填（或用 QWEN2API_PROXY_PASSWORD）"
  }
}
```

- `{account}` 替换成账号名（`main` → `poolclient-main`），`{password}` 取 Password 字段；
- **只支持 `socks5` / `socks5h`**：我们要的是"换出口 IP"，HTTP CONNECT 代理常带
  `X-Forwarded-For`，起不到隔离作用，所以直接拒绝；
- `socks5h` 语义 = 域名交给代理解析（远端 DNS）。本仓的 SOCKS5 客户端对域名一律用
  `ATYP=domain`，所以不会出现"本地解析成错区域的 IP、连得上但被风控"这种隐形故障；
- 每个账号一条 `http.Transport`（连接复用 + 出口稳定），**按账号缓存**，不随 token 变化
  （换 token 不该换 IP）；
- 登录（signin / refresh）、上传、生成、余额读取**全部**走该账号的出口 —— 登录 IP 与
  使用 IP 不一致本身就是风控画像里的可疑信号。

### 没有账号归属的请求怎么办

公共端点（未登录探测）没有账号上下文：

| `direct_when_missing` | 行为 |
|---|---|
| `true`（默认） | 直连。适合"代理是可选项"的部署 |
| `false` | 直接拒绝。适合"绝不允许真实 IP 出现在上游"的部署 |

### 出口 IP 怎么看（实测，不靠猜）

代理 host 对所有账号都是同一个（`pool.livetest.cn:2090`），**出口 IP 由运营商侧按账号分配**，
光看配置根本看不出这次是从哪出去的。所以网关做了一次"懒探测"：

- 某个账号第一次被用来发请求时，后台从**它的隧道**请求一次 `upstream.egress_ip_url`
  （默认 `https://api.ipify.org`，可换，设 `"-"` 关闭），把对面看到的来源 IP 存进池；
- 缓存 **30 分钟**，过期后下一次请求顺带重探；全程**异步**，不占主链路延迟；
- 结果落在两处：请求流水的 `egress_ip`（面板「日志」→ 来源/出口、复制/导出都带），
  以及账号快照的 `egress_ip`（`/stats` 可见）。

> 排障口径：`client_ip` 是"谁调我们"，`egress_ip` 是"我们从哪个 IP 访问上游"。
> 同一个上游报错，如果只在某个 `egress_ip` 上稳定复现，那就是 IP 画像问题，不是账号问题。

### 实测（2026-10-30）

同一个代理池、不同账号名 → 不同且稳定的出口：

```
probe-a → 38.6.213.75   （连两次都一致）
probe-b → 38.6.213.41
```

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
| 控制台 | ✅ 账号/流水/对话框调试（文本/图片/视频）正常渲染 |
| **文生图** `/v1/images/generations` | ✅ 200，83.8s；`url` 模式返回 `cdn.qwenlm.ai/.../t2i/….png?key=<JWT>` |
| **图生图** `image` = data URI | ✅ 50.4s；产物与原图同构图、按提示词变水彩（人工核对） |
| **图生图 · 双参考图** | ✅ 136.7s / 2048²；"左红右蓝"精确执行（两张图都被消费，目视核对） |
| **尺寸矩阵** 1:1 / 16:9 / 9:16 / auto | ✅ 2048² / 2752×1536 / 1536×2752 / 2048² —— **比例真的生效** |
| **`n=2`** | ✅ 2 张，165.7s（串行两次上游调用，约 83s/张） |
| **额度耗尽** | ✅ 429 + `insufficient_quota` + 上游原话（识别 `extra.update_member`） |
| **每日额度（免费号）** | ✅ 全程压测：**9 张**后耗尽；中途 2 次「静默挂起」但自愈（非额度） |
| **挂起保护** | ✅ 图片单次预算 = `image_timeout_s`（默认 **300s**，真跑实测出图 60s、最慢双垫图 137s）；超时 → 该号冷却 10 分钟 + **直接回 504**（不换号重试：预算已很长，再等一轮等于挂 10 分钟）。"额度将尽 / 上游故障 / 只是慢"三种可能只写流水，不替调用方猜 |
| **超时后取回** | ✅ 504 的流水写明"补捞几次 + 为什么没救回来"（如 `会话补捞 2 次（链接已落库但未定稿，CDN 未签名）`）+ `chat_id`；面板请求表点 chat_id 即可复制，拿去上游会话历史手工取图（2026-11-02 线上实测取回 2752×1536 PNG） |
| **本地每日配额** | ✅ 限额设 1 实测：第 1 张 69.9s 出图，第 2 张 **0.003s 直接 429**（未打上游） |
| **图生图 b64** `response_format:"b64_json"` | ✅ 53.9s，9.95MB base64 → 7.46MB PNG（`89 50 4E 47` 校验） |
| **`/v1/images/edits`**（multipart） | ✅ 51.7s，`-F image=@ref.png` 直接出图 |
| **文件解析** `/v1/files` + chat | ✅ 上传 95B txt → 提问 14.8s，原样答出文件内关键词 |
| **联网搜索** | ✅ `thinking:auto` + 明确要求，82s 返回当日科技新闻（带来源与 `[[40]]` 引用） |
| **Agent 模式（上游契约）** | ✅ 浏览器实测：一句「写 hello.py 并运行」→ 4 轮工具（`write_file` + 3×`bash`）、自主纠错（`cd /home/user` 失败后 `pwd`/`find` 找到真实路径）、11 秒给出完整路径；usage 39,138/697（cache 34,816） |
| **Agent 模式（网关转换）** | ✅ 线上实测（0.0.60）：`content="42"`，流水 `tools=bash({"command":"echo 42"}) → completed exit=0 stdout=42`，usage 13,889/72（reasoning 36），14.0s；流式同验（`reasoning_content` 184 字 + `[DONE]` 正常） |
| **文生图 b64** `response_format:"b64_json"` | ✅ 71.4s，7.5MB base64（含 CDN 就绪重试，见下） |
| **文生视频** `POST /api/v3/contents/generations/tasks` | ✅ 2.7s 拿到 `tsk_…`（回包只有 `id`） |
| **视频轮询 + 产物** | ✅ 约 15 分钟 `queued → in_progress → completed`，产物 `cdn.qwenlm.ai/.../t2v/….mp4`，1.7MB 可下 |

## 视频按积分计费（余额是唯一真口径）

上游视频是**积分制**，不是一个价：720P 5s 与 1080P 15s 差 4 倍。

### 价格（按模型版本分两套）

| 档位 | `happyhorse11`（默认） | `happyhorse` 1.0 |
|---|---|---|
| 720P 5s / 10s / 15s | **30 / 60 / 90** | 40 / 80 / 120 |
| 1080P 5s / 10s / 15s | **40 / 80 / 120** | 70 / 140 / 210 |

- 官方来源：`GET /api/v2/credits/pricing`（`scene` 里 `hh11*` 是新版、`hh*` 是旧版）；
- 实测对账：账单 `HappyHorse1.1.720P.5s = -30`、`1080P.15s = -120`，与 hh11 表一致；
- 查不到的档位直接 **400**，不猜价（宁可拒绝，也别按错的单价放行）。

### 余额：直接读账号账本，不靠配置猜

网关会读**账号真实余额**（`sg-entitlement.qwen.com`，见 `docs/UPSTREAM.md` §4.10）：

```
GET /stats → pool[].credit_balance = 280, credit_known = true
```

选号时按"余额 ≥ 本次所需"过滤。这解决了一个真实问题：同样叫"普号"，
余额可能是 40（今天刚发）、10（昨天剩的）、270（会员订阅）—— 配置里写死一个数必然错。

```
余额 10  + 请求 720P 5s(30)  → 429「账号积分余额不足：本次需要 30 积分，各账号余额 main=10」
余额 280 + 请求 720P 5s(30)  → 200，扣 30，余额立刻变 250（乐观扣减，不等下一次刷新）
```

- 余额缓存 **60s**（视频路径）/ **2 分钟**（`/stats` 面板轮询），并发只跑一轮刷新；
- 读不到余额 ⇒ **不拦截**，交给上游 `insufficient_quota` 兜底（一次接口抖动不该停掉视频能力）；
- 提交成功立刻**乐观扣减**，避免同一分钟内的第二个请求看到过期余额。

### 配置

```json
"pool": { "max_video_per_day": 1, "max_video_credits_per_day": 0 },
"video": {
  "root_model": "happyhorse11",
  "pricing": { "720P": { "5": 30, "10": 60, "15": 90 },
               "1080P": { "5": 40, "10": 80, "15": 120 } }
}
```

`max_video_credits_per_day` 默认 **0（关）**：余额已经是真口径，再叠一层
"40 分/天"会把会员号的 270 分锁死。想额外加日上限再填正数。

### 去水印与自建托管（`storage`）

上游产物的**右下角烧着固定 133×33 的 Qwen 图层**（不是等比缩放，是锚定右下角的
固定像素图层），且 CDN 直链 **24h 就过期**。启用 `storage` 后链路变成：

```
上游 succeeded ──► polishing（内部状态）──► 去水印 ──► 上传 ──► succeeded + 自建 URL
                          │
                          └─► 对外一律报 running（绝不提前给带水印的直链）
```

三条纪律：

| 纪律 | 为什么 |
|---|---|
| 没处理完不发 URL | 带水印那版 24h 后 404，事后没有补救窗口 |
| `polishing` 对外映射为 `running` | 方舟只有六态，自造状态会让按状态机写死的客户端永远等不到终态 |
| 失败**不**降级透传上游直链 | 与其交一个会过期且带水印的瑕疵品，不如明确失败 |

去水印不是调参数能解决的：`biz_data` 里根本没有 watermark 字段，
水印是在渲染链路上烧进像素的，只能后处理重绘。实现要点：

- **不能用 ffmpeg `delogo`**：它做双向插值，133px 的水平跨度会把左右边界高光
  整条拉进画面中部（实测在深色木桌上拉出明显竖直亮条）。宽条形要**垂直优先**插值。
- 采样带外移 2~3px（紧贴 bbox 的行仍带抗锯齿余辉）+ 横向中值滤波（治"单列被染亮"）
  + 噪声匹配（填充区过干净反而显眼）。
- bbox 取偶必须**向外**：`h -= h%2` 会把 41 缩成 40，水印底边 697 只到 696，
  留 1px 白点。

配置（**凭据走环境变量，别写进仓库**）：

```json
"storage": {
  "enabled": true,
  "endpoint": "https://oss.s3ai.cn",
  "bucket": "cdn",
  "public_base": "https://oss.s3ai.cn/cdn",
  "prefix": "qwen2api/videos",
  "strip_watermark": true,
  "quality": "near",
  "workers": 2
}
```

```bash
QWEN2API_STORAGE_ACCESS_KEY=... QWEN2API_STORAGE_SECRET_KEY=...
```

画质档实测（5.16s / 720p / 源 922KB）：`near` 6.1MB / 1.7s（默认，视觉无损）；
`lossless`（qp=0）21.7MB / 2.1s —— 后者是唯一能让**水印区外像素逐位不变**的档，
已用解码帧比对验证差值为 0。

⚠️ 代价：镜像要装 ffmpeg，从 ~20MB 涨到 ~100MB。

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
- **视频任务映射已落盘**：`data/videos.json`，保留 8 天（上游任务 ID 活 7 天）。
  早前只存内存 2h，重启即丢客户端手里的 id —— 那是契约破坏，已修。
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
