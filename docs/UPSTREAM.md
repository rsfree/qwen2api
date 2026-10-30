# chat.qwen.ai 网页端上游契约

> 本文件是本仓**上游侧的唯一真源**：`internal/upstream/*` 里凡与上游形态相关的判读，
> 都能在这里找到依据与证据等级。改实现之前先看这里；改完实现记得回来更新。

**取证时间：2026-10-28**（浏览器实测，Chrome 156 / macOS，已登录账号）。
上一版公开契约（rsfree/qwen，2026-09-24）在**鉴权、version 头、模型端点**三处已经失效，
差异见 §9。

证据等级：✅ **实测**（真实请求 + 读到响应）／📖 **静态取证**（前端 bundle / 响应头）／
⚠️ **未证实**（不得在实现里假设）。

---

## 0. 一句话模型

```
POST /api/v2/chats/new                      → data.id（chat_id）
POST /api/v2/chat/completions?chat_id=<id>  → SSE 增量流（response.created → thinking_summary → answer → finished）
GET  /api/v2/models/                        → data.data[]（需凭据；7 个 t2t 模型）
GET  {auth}/api/v2/auths/                   → 账号资料（验活用）
GET  {auth}/api/v2/auths/refresh            → 刷新登录态（Set-Cookie）
```

- 会话**必须先建**：`completions` 的 `chat_id` 指向一个已存在会话。
- 流式响应**没有** `data: [DONE]`：结束判据是 `delta.status=="finished" && delta.phase=="answer"`。
- 查询类端点的 HTTP 状态可能是 200，真码在响应头 `x-actual-status-code`。

---

## 1. 端点

| Method | 路径 | 用途 | 证据 |
|---|---|---|---|
| `POST` | `/api/v2/chats/new` | 建会话，返回 `data.id` | ✅ |
| `POST` | `/api/v2/chat/completions?chat_id=<id>` | 提交一轮对话，SSE 返回 | ✅ |
| `GET` | `/api/v2/models/` | 模型清单（**需凭据**） | ✅ |
| `GET` | `/api/v2/users/user/settings` | 用户设置（含工具开关） | ✅ |
| `GET` | `/api/v2/credits/pricing` | 会员与积分定价（读额度口径用） | ✅ |
| `GET` | `{auth}/api/v2/auths/` | 账号资料（id/email/name/role） | ✅ |
| `GET` | `{auth}/api/v2/auths/refresh` | 用 cookie 换 `access_token`（Bearer） | ✅ |
| `POST` | `/api/v2/users/status` | 登录态探活（`data: true`） | ✅ |

- `{auth}` = `https://auth.qwen.ai` —— **鉴权域已与业务域 `chat.qwen.ai` 分离**（旧契约里两者同域）。
- 旧契约的 `GET /api/models` 已不存在，现在是 `/api/v2/models/`，且响应体多包了一层 `data`
  （`{"data":{"data":[...]}}`）。

---

## 2. 鉴权与请求头

### 2.1 两段式登录：signin(cookie) → refresh(access_token)

✅ 全链路实测（2026-10-28），**这是本仓最重要的一节**：

```
① POST https://chat.qwen.ai/api/v2/auths/signin
   body: {"email":"…","password":"<sha256hex(明文)>"}
   → 200 + Set-Cookie: token=<JWT，209 字符>；body 是账号资料（**没有 token**）

② GET https://auth.qwen.ai/api/v2/auths/refresh
   Cookie: token=<上面那串>
   额外头：x-request-origin: https://chat.qwen.ai
          sec-fetch-site: same-site（写 same-origin 会 "Invalid request header"）
   → 200 + data.access_token（281 字符）← **这才是能调写端点的 Bearer**

③ Authorization: Bearer <access_token>  → 所有 /api/v2/* 正常
```

🔴 **cookie token 只能读、不能写**（实测同一个 token）：

| 端点 | `Cookie: token=` | `Authorization: Bearer` |
|---|---|---|
| `GET /api/v2/models/` | ✅ 200 | ✅ 200 |
| `POST /api/v2/chats/new` | ❌ `Unauthorized` | ✅ 200 |
| `POST /api/v2/chat/completions` | ❌ | ✅ |

🔴 **两种形态不能同时发**：带 `Authorization: Bearer <无效值>` 时服务端**不再回退**去读
Cookie，直接 401。所以网关必须显式确定形态（本仓用 `cookie:` / `bearer:` 前缀 + 启动探测）。

- 凭据失效的三种表象（都必须判为鉴权失效）：
  - HTTP 401 / `{"success":false,"data":{"code":"Unauthorized","details":"您没有权限访问此资源…"}}`；
  - HTTP 200 + `{"code":"unauthorized","details":"Token has expired, please log in again."}`；
  - `auths/refresh` 返回 `Bad_Request: Missing origin`（说明缺 `x-request-origin`，不是凭据问题）。

### 2.2 请求头（实测逐字段）

```
accept: application/json, text/plain, */*
accept-language: zh-CN,zh;q=0.9
authorization: Bearer <token>
bx-ua: <设备指纹，可缺>
bx-umidtoken: <设备指纹，可缺>
bx-v: 2.5.37
content-type: application/json
origin: https://chat.qwen.ai
referer: https://chat.qwen.ai/c/<chat_id>
sec-ch-ua: "Not:A-Brand";v="8", "Chromium";v="156", "Google Chrome";v="156"
sec-ch-ua-mobile: ?0
sec-ch-ua-platform: "macOS"
sec-fetch-dest: empty
sec-fetch-mode: cors
sec-fetch-site: same-origin
source: web
timezone: Wed Oct 28 2026 17:48:27 GMT+0800
user-agent: <Chrome 156 macOS>
version: 0.3.26
x-request-id: <uuid4>
```

- 🔴 **`version` 必须是当前前端版本**：实测 0.3.26（旧契约写 0.2.0）。这个头缺失或过期
  会得到 `Bad_Request` 文案，**看起来像请求体写错**，极易误诊。
- `timezone` 的格式是 `GMT+0800`，不是时区名（`CST`）。
- 🔴 **`completions` 的 RGV587 门槛：请求头必须"齐"**（2026-10-28 现场踩坑，最终打通）
  - 症状：`POST /api/v2/chat/completions` 返回
    `{"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::哎哟喂,被挤爆啦,请稍后重试"],
      "data":{"url":"…/punish?x5secdata=…&action=captcha"}}`（HTTP 200 + JSON，**不是 SSE**）。
  - 页面在 completions 上**显式设置的头只有 8 个**（hook 实证）：
    `Accept` · `Accept-Language` · `Content-Type` · `version` · `source` · `timezone` ·
    `X-Request-Id` · **`X-Accel-Buffering: no`**
  - **`X-Accel-Buffering: no` 是最容易漏的一个**（看起来只是给 nginx 的缓冲指令）。
    补齐这一组后，同样的账号/同样的 TLS 栈（Go 原生）立即打通。
  - `bx-ua` / `bx-umidtoken` 仍然是浏览器层注入的（JS 层看不到，说明由扩展/WindVane 加），
    本仓按页面抓到的形态发 `defaultFY2_load_failed with timeout@@<origin>@@<ms>`。
  - ⚠️ **未做单变量拆分**：不能断言"只有 X-Accel-Buffering 起决定作用"，
    只能说"补齐这 8 个显式头 + 2 个 bx 头"的组合可稳定通过。
  - 注意 `chats/new` 的阈值明显更低：缺头也能建会话，**只有 completions 会拦** ——
    所以"建会话成功"不能证明头齐了。
- ⚠️ 风控文案是「被挤爆啦」，看起来像限流；实测**等 15 分钟也不会自己好**，
  换账号也没用（两个不同账号同样被拦）⇒ 是请求特征判定，不是账号状态。

### 2.3 官方允许的请求头（来自 `auths/refresh` 的 CORS 响应头）📖

```
access-control-allow-headers: Authorization, Content-Type, x-request-id,
  x-request-origin, timezone, source, user-agent, version, bx-ua, bx-umidtoken
access-control-allow-origin: https://chat.qwen.ai
```

这份白名单反过来印证了 §2.2 里的关键头都是**服务端明确接受**的。

---

## 3. 请求体

### 3.1 `POST /api/v2/chats/new`

```json
{
  "title": "New Chat",
  "models": ["qwen3.7-plus"],
  "chat_mode": "normal",
  "chat_type": "t2t",
  "timestamp": 1793180907875,
  "project_id": ""
}
```

响应：`{"success":true,"request_id":"…","data":{"id":"8f200cc1-…"}}`
（`timestamp` 是**毫秒**）

### 3.2 `POST /api/v2/chat/completions?chat_id=<id>`

✅ 实测逐字（2026-10-28，删去了本机 id）：

```json
{
  "stream": true,
  "version": "2.1",
  "incremental_output": true,
  "chatId": "<chat_id>",
  "parentId": "<上一条消息 id，首轮为 \"\">",
  "chat_id": "<chat_id>",
  "chat_mode": "normal",
  "model": "qwen3.7-plus",
  "parent_id": null,
  "timestamp": 1793181392,
  "messages": [{
    "id": null,
    "fid": "<uuid4>",
    "parentId": "<同上>",
    "childrenIds": ["<uuid4>"],
    "role": "user",
    "content": "只回复：ok",
    "user_action": "chat",
    "files": [],
    "timestamp": 1793181392,
    "models": ["qwen3.7-plus"],
    "model": "",
    "chat_type": "t2t",
    "feature_config": {
      "thinking_enabled": true,
      "output_schema": "phase",
      "research_mode": "normal",
      "auto_thinking": true,
      "thinking_mode": "Auto",
      "thinking_format": "summary",
      "auto_search": true
    },
    "extra": {"meta": {"subChatType": "t2t"}},
    "sub_chat_type": "t2t",
    "parent_id": "<同上>"
  }]
}
```

🔴 五个易错点（每条都对应一次真实故障）：

1. **`chatId` 与 `chat_id` 必须双写**（顶层）。缺小写 ⇒ `RequestValidationError: Field 'chat_id': Field required`。
2. **`messages[0].id` 必须存在且为 `null`**，不能省略。
3. **`messages[0].files` 恒在**，纯文本为 `[]`（不是 `null`）。
4. **`timestamp` 是秒**（建会话是毫秒）。
5. 顶层 `version:"2.1"` 与 HTTP 头 `version:"0.3.26"` 是**两个不同的东西**，别混。

### 3.3 思考档位（前端「自动 / 思考 / 快速」）

| 档位 | thinking_enabled | thinking_mode | auto_thinking | thinking_format | 说明 |
|---|---|---|---|---|---|
| `auto`（默认） | true | `Auto` | true | `summary` | 模型自己决定是否思考 |
| `thinking` | true | `Thinking` | false | `summary` | 强制思考 |
| `fast` | false | `Fast` | false | **不带** | 关思考，首字最快 |

三档共同：`output_schema:"phase"`、`research_mode:"normal"`、`auto_search:true`。

### 3.4 多轮上下文 = `chat_id` + `parent` 链

✅ 实测：第二轮请求的 `parentId` / `messages[0].parentId` / `messages[0].parent_id`
全部等于**上一轮回复的消息 id**，`messages` 数组里**只放本轮这一条** —— 历史由服务端按
parent 链维护。

对本仓的含义：**网关是无状态设计**（每请求新建会话），所以必须把历史拍平成单条 prompt。
若将来要做"复用会话省 token"，正确做法是缓存 `chat_id` 与最后一条消息 id，而不是重发历史。

---

## 4. SSE 帧形态（✅ 实测，单轮完整样本）

### 4.1 帧类型

| # | 帧 | 判据 | 处理 |
|---|---|---|---|
| 1 | `response.created` | 顶层有 `response.created` | 记 `chat_id` / `parent_id` / `response_id` |
| 2 | 思考摘要 | `choices[0].delta.phase == "thinking_summary"` 且 `status == "typing"` | `extra.summary_title/summary_thought` **累加数组**，需 diff |
| 3 | 思考结束 | 同 phase，`status == "finished"` | **不是**整条流结束 |
| 4 | 正文增量 | `phase == "answer"` 且 `status == "typing"` | `delta.content` 是**增量** |
| 5 | 收尾 | `phase == "answer"` 且 `status == "finished"` | 整条流结束 |
| — | `data: [DONE]` | **不存在** | 别等它 |

### 4.2 原始样本

首帧：

```
data: {"response.created": {"chat_id": "8f200cc1-…", "parent_id": "c4ac4793-…", "response_id":"a0ed796f-…", "response_index": "0"}}
```

思考摘要（`extra` 是累加数组，不是增量）：

```
data: {"choices": [{"delta": {"role": "assistant", "content": "", "phase": "thinking_summary",
  "extra": {"summary_title": {"content": ["解析用户指令并执行计算"]},
            "summary_thought": {"content": ["我首先审视了用户的提问与限制条件…"]}},
  "status": "typing"}}], "response_id": "a0ed796f-…",
  "usage": {"input_tokens": 1239, "output_tokens": 105, "characters": 0, "total_tokens": 1344,
            "input_tokens_details": {"text_tokens": 1239},
            "output_tokens_details": {"reasoning_tokens": 99, "text_tokens": 105}},
  "timestamp": 1793181438}
```

思考结束：

```
data: {"choices": [{"delta": {"role": "assistant", "content": "", "phase": "thinking_summary", "status": "finished"}}], "response_id": "a0ed796f-…", "timestamp": 1793181438}
```

正文增量：

```
data: {"choices": [{"delta": {"role": "assistant", "content": "2", "phase": "answer", "status": "typing"}}], "response_id": "a0ed796f-…", "usage": {…}, "timestamp": 1793181438}
```

收尾：

```
data: {"choices": [{"delta": {"content": "", "role": "assistant", "status": "finished", "phase": "answer"}}], "response_id": "a0ed796f-…"}
```

### 4.3 `usage`

- 挂在**帧顶层**（不在 `choices` 里），随 answer 帧出现且**逐帧递增**，最后一份即终值。
- `input_tokens` / `output_tokens` / `total_tokens` 直接可用；
  `output_tokens_details.reasoning_tokens` 是思考 token（可透出为 `completion_tokens_details`）。
- 思考阶段的帧也带 usage，但那时 `output_tokens` 已含思考消耗 —— **不要累加，取最后一份**。

### 4.4 流内错误（HTTP 200 包着错误）

两种形态，都必须**响亮失败**：

```
data: {"error": "Internal error!"}
data: {"error": {"code": "invalid_input", "details": "…"}}
```

---

## 4.5 图片生成：两条完全不同的路（✅ 2026-10-28）

同一个 `POST /api/v2/chat/completions`，但**按 chat_type 分派到两套不同的产物通道**——
这是本仓踩得最深的一处，写错任何一条都会得到"HTTP 200 + 空内容"。

### 4.5.1 t2i（显式生图）→ `phase:"image_gen"`，content 就是直链

请求体关键字段（三处 chat_type 必须一致，`extra.meta.model` 不能少）：

```json
{
  "chat_type": "t2i",
  "sub_chat_type": "t2i",
  "extra": {"meta": {"subChatType": "t2i", "size": "16:9", "model": "qwen-image-3.0"}},
  "feature_config": {"thinking_enabled": false, "thinking_mode": "Fast", ...}
}
```

产物帧（**一帧一张**）：

```json
{"choices":[{"delta":{
  "role":"assistant",
  "content":"https://cdn.qwenlm.ai/output/<uid>/t2i/<response_id>/<ts>.png?key=<JWT>",
  "phase":"image_gen",
  "extra":{"output_image_hw":[["928","1664"]],"model":"qwen-image-3.0"},
  "status":"typing"}}],"response_id":"…"}
```

结束帧：`{..., "phase":"image_gen", "status":"finished"}` —— 它**不是**整条流结束。

🔴 三个反直觉点（每条都实测过）：

1. **`delta.content` 是裸 URL**，不是 markdown、不是 JSON。当成正文吐给客户端 = 回复里突然出现一条链接。
2. **`extra.meta.model` 必须传**（`qwen-image-3.0`）。不传时上游只回
   `response.created` + 每 10s 一次的 `response.info{action:"keep_alive"}`，
   **一个内容帧都没有**，直到超时。
3. **不要在 prompt 里加"请画一张图："之类的引导**。t2i 是显式类型，上游按类型分派；
   被改写的 prompt 会让 t2i 分支拿不到有效绘图提示词，同样静默空转。

### 4.5.2 t2t 里的工具调用 → `phase:"image_gen_tool"`

不加 `chat_type`（走普通对话）时，模型可能**自己决定**调 `image_gen` 工具：

```json
{"choices":[{"delta":{"phase":"image_gen_tool","status":"typing",
  "function_call":{"name":"image_gen","arguments":"{\"prompt\": \"A cute ginger cat, …\"}"},
  "function_id":"call_…","extra":{}}}]}
```

`arguments` 是**逐帧全量快照**（不是增量），中途常常是非法 JSON，必须静默跳过等最后一帧。
产物在结束帧（`role` 变成 `"function"`）：

```json
{"choices":[{"delta":{"role":"function","phase":"image_gen_tool","status":"finished",
  "name":"image_gen","extra":{
    "tool_result":[{"image":"https://cdn.qwenlm.ai/…/t2i/1req_….png?key=…"}],
    "image_list":[{"image":"https://cdn.qwenlm.ai/…/image_gen/<response_id>/<ts>.png?key=…"}],
    "display_position":"answer"}}}]}
```

两处都给 URL（`image_list` 是最终产物、`tool_result` 是工具原始返回），去重合并。

**选哪条**：要"一定出图"就用 t2i（本仓 `/v1/images/generations` 走这条）；
只想让模型自由发挥就用 t2t。

### 4.5.3 尺寸：比例被采纳，且是"真的按比例出图"（✅ 2026-10-28 多形态实测）

同一个 prompt（"a simple red circle on white background"）只改 `size`，产物实测：

| 请求 size | 实际输出 | 宽高比 | 耗时 |
|---|---|---|---|
| `1:1` | **2048 × 2048** | 1:1 ✓ | 64.6s |
| `16:9` | **2752 × 1536** | 1.79 ✓ | 65.4s |
| `9:16` | **1536 × 2752** | 0.558 ✓ | 59.2s |
| `auto` | **2048 × 2048** | 模型自选 | 54.4s |

⇒ 比例枚举**确实生效**（不是被忽略后统一出方图）。这与 image-adapter 2026-09-23 的
"16:9→2688×1536" 数值略有差异（本仓实测 2752×1536），说明画布尺寸由模型版本决定，
**只有比例是稳定契约**。所以网关只翻译比例，不承诺像素。

`n > 1` 是**串行多次上游调用**（一次工具调用只出一张）：`n=2` ⇒ 165.7s / 2 张，约 83s/张。

### 4.5.4 🔴 第三条错误通道：额度耗尽伪装成 answer（✅ 2026-10-28 实测）

这条最容易漏，因为**它长得完全不像错误**：

```json
{"choices":[{"delta":{
  "role":"assistant",
  "content":"今日生图额度已用完，升级会员可获得更多生图额度。",
  "phase":"answer","status":"finished",
  "extra":{"update_member": true}}}],"usage":{}}
```

HTTP 200、SSE 正常收流、`phase` 是普通的 `answer`。识别标志只有 `extra.update_member`。

不识别它的后果：落到"没拿到图"分支，报成**"模型未触发 image_gen，请把 prompt 写明确"** ——
把"该充值/换号"误导成"该改写措辞"，实测踩过一次。

本仓处置：`ErrQuota` ⇒ HTTP **429** + `type: insufficient_quota` + **上游原话透传**。

> 至此错误共有三条投递通道，都要读：
> ① HTTP 4xx/5xx；② 流内 `{"error":…}` 帧；③ **answer 里的一句提示 + `extra.update_member`**。

### 4.5.5 每日生图额度与"静默挂起"（✅ 2026-10-28 单号全程实测）

对一个**全新免费账号**连续压测到它拒绝，完整画像：

| 阶段 | 表现 |
|---|---|
| 前 9 张 | 正常出图，单张 **59~87s** |
| 中途第 4、5 次 | **静默挂起**（只发 keep_alive，240s 无产物）→ 但**几分钟后自愈**，继续出图 |
| 第 10 张 | 又挂起（180s 心跳） |
| 第 11 次 | **明确拒绝**：`今日生图额度已用完，升级会员可获得更多生图额度`（`extra.update_member`） |

⇒ **免费号每日上限约 9 张**（本号 9 张后耗尽）。
⇒ 🔴 **"挂起"是双义的**：既可能是额度将尽的前兆，也可能是上游临时故障。
   本号第 4、5 次挂起后恢复并又出了 6 张 —— 所以**不能一遇挂起就判额度尽**。
   区分方法：隔几十秒**再发一次** —— 拿到 `insufficient_quota` 才算确认；拿到图就是临时故障。
⇒ 额度文案是「**今日**」⇒ 按自然日重置（不是 24h 滚动）。本仓未跨日验证，标注为待确认。
⇒ 会员号上限明显更高：另一账号（有会员）当日跑 6 张仍在正常出图中，未探到上限。

**网关侧处置**（`internal/server/images.go`）：
- 图片请求硬超时 **180s**（正常 55~87s，留足余量），避免调用方跟着上游无限空等；
- 超时返回的文案**不武断**，写明"两种可能 + 怎么区分"，而不是报"额度已用完"误导。

### 4.5.6 产物直链的两个坑（✅ 2026-10-28）

| 现象 | 原因 | 处置 |
|---|---|---|
| 刚出图立刻下 ⇒ **404 + 226 字节 HTML** | CDN 异步转存未就绪 | 退避重试（1s/3s）；或稍后再取 |
| 带 `Referer` 才能下（视频尤其） | CDN 防盗链 | 请求加 `Referer: https://chat.qwen.ai/` |
| `Content-Type` 不是 `image/*` | 真图可能回 `application/octet-stream` | **按 magic bytes 判**（PNG `89 50 4E 47` / MP4 `ftyp`） |

> 这三条来自 image-adapter 的既有结论 + 本次复现：同一账号同一 URL，18:26 下 404、
> 19:11 下 200 且 8.5MB —— 不是签名失效，是转存延迟。

## 4.6 图生图 / 图片编辑：先上传，再 `image_edit`（✅ 2026-10-28 本仓实测）

外链 URL 上游**不认**，参考图必须先传进它自己的 OSS。三步（契约来自 `rsfree/qwen` 的
`app/upstream/qwen/upload.py`，image-adapter 侧亦有 20 张输入图实测）：

### 4.6.1 `POST /api/v2/files/getstsToken`

```json
{"filename": "ref.png", "filesize": "2931", "filetype": "image"}
```

- `filesize` 是**字符串**；`filetype` ∈ `image`/`video`/`audio`/`file`（按 MIME 前缀判）。
  形态不对 ⇒ `Bad_Request: Invalid file information!`（不是"不支持"）。
- 返回 `data{access_key_id, access_key_secret, security_token, bucketname, endpoint,
  file_path, file_url, file_id, region}`。
- 🔴 **每张图一次**：STS 凭证被 `SessionPolicy` 钉在那一个 key 上，
  拿 #1 的凭证签 #2 的路径会 `403 AccessDenied`。

### 4.6.2 `PUT https://{bucket}.{endpoint}/{file_path}`（OSS V4 头签名）

- 派生密钥前缀是 **`aliyun_v4`**（scope 尾段才是 `aliyun_v4_request`）；
- `region` 要剥掉返回值的 `oss-` 前缀；
- 必须签 `x-oss-security-token`（STS 临时凭证）与 `x-oss-content-sha256: UNSIGNED-PAYLOAD`。

本仓用一条**跨语言基准测试**钉住签名（Python 独立算 → Go 对照），
因为签错只会得到 403，从错误信息看不出哪一步错了。

### 4.6.3 提交：`chat_type = image_edit`

```json
{"chat_type": "image_edit", "sub_chat_type": "image_edit",
 "messages": [{"chat_type": "image_edit", "sub_chat_type": "image_edit",
   "files": [{"type":"image","file":{},"id":"…","url":"<file_url>","name":"ref.png",
              "collection_name":"","progress":100,"status":"uploaded","greenNet":"success",
              "size":2931,"error":"","itemId":"…","file_type":"image/png",
              "showType":"image","file_class":"vision"}], ...}]}
```

🔴 **`files[]` 的字段一个都不能省**：少 `status`/`greenNet`/`itemId` 会被当成"附件没传完"而
**静默忽略** —— 请求照常 200，但图根本没进去。
🔴 **`image_edit` 不带 `size`**（顶层与 `extra.meta` 都不带），这是与 t2i 的关键差异。

产物仍走 `phase:"image_gen"`（`delta.content` = 直链），与 t2i 同一条读法。

**实测**：蓝底黄圆的 512² PNG + "把这张图变成水彩风格" ⇒ 50.4s 拿到产物，
构图一致、渲染成水彩纸纹（人工核对）；`b64_json` 模式 53.9s / 7.46MB PNG。

## 4.7 视频生成：`tsk_` 异步任务（✅ 2026-10-28）

与文本/图片**完全不同的读法**：`stream:false` 同步回一个任务 id，产物靠轮询。

```
POST /api/v2/chat/completions?chat_id=<id>   (stream:false)
  → {"success":true,"data":{"messages":[{"extra":{"wanx":{"task_id":"tsk_…"}}}]}}
GET  /api/v2/task/status/<task_id>
  → 任务对象（task_status / content 等）
```

请求体（注意 `sub_chat_type` 与 `chat_type` **不同值**）：

```json
{
  "chat_type": "t2v",
  "sub_chat_type": "zaodian_generate_video",
  "extra": {"meta": {
    "size": "16:9",
    "subChatType": "zaodian_generate_video",
    "videoGenerationMode": "standard",
    "biz_data": "{\"bizScene\":\"genVideo\",\"req\":{\"rootModel\":\"happyhorse11\",\"genMode\":\"vid_gen\",\"params\":{\"resolution\":\"720P\",\"size\":\"16:9\",\"duration\":5,\"audio\":true}}}"
  }},
  "size": "16:9"
}
```

🔴 四个硬约束：

1. **`stream` 必须是 `false`** —— t2v 不接受流式。
2. **`sub_chat_type` = `zaodian_generate_video`**（不是 `t2v`），`extra.meta.subChatType` 同值。
   按 chat_type 复制过去会静默失败。
3. **`biz_data` 是字符串化的 JSON**，不是对象。
4. 任务 id 前缀是 **`tsk_`**（旧契约写的 `cgt_` 是方舟风格，这里不是）。

档位（来自 `credits/pricing` 的计价表）：

| resolution | duration | 积分 |
|---|---|---|
| 720P | 5s | 40 |
| 720P | 10s | 80 |
| 720P | 15s | 120 |
| 1080P | 5s | 70（走付费） |

## 4.7 文件解析：`t2t` + `files[]`（✅ 2026-10-28 本仓实测）

上传链与图生图共用（§4.6.1 / §4.6.2），差别只在 `filetype` 与条目映射：

| MIME 前缀 | `filetype` | `showType` | `file_class` |
|---|---|---|---|
| `image/*` | `image` | `image` | `vision` |
| `video/*` | `video` | `video` | `video` |
| `audio/*` | `audio` | `audio` | `audio` |
| 其余（PDF/txt/docx…） | `file` | `file` | `document` |

📌 **关键结论：文件解析不需要特殊 `chat_type`** —— `t2t` + `messages[0].files` 即可。
（与图片不同：图片要 `t2i`/`image_edit` 分流。）

**实测**：95 字节 `test_doc.txt`（内容含「紫色大象 42」）上传后，用
`t2t` + `files` 提问"这个文件里写了什么关键词" ⇒ 模型原样答出「紫色大象 42」，
`input_tokens` 12679（文件被读入上下文）。

## 4.8 联网搜索：`feature_config.auto_search`（✅ 2026-10-28）

不需要额外端点，也不需要 `chat_type`：

- `feature_config` 里 `auto_search: true`（本仓三档都带，与网页端一致）；
- 但**是否真的检索由模型决定**：`thinking: fast` + 泛问（"某天收盘点位"）会得到
  "我无法提供实时数据"；`thinking: auto` + 明确要求（"请联网搜索今天的科技新闻"）
  则会返回带来源链接与 `[[40]]` 引用的结果。

⇒ 网关侧不需要开关，把 `auto_search` 保持 true 即可；调用方要搜索时**在 prompt 里明说**更可靠。

## 4.9 TTS / 实时语音：**没有可网关化的端点**（✅ 2026-10-28 取证结论）

这一节是**否定结论**，写下来是为了以后不再重复摸索。

### 4.9.1 `GET /api/v2/tts/config` 只是能力目录

```
GET /api/v2/tts/config?omni_speakers=v1&audio_tts_speakers=v1&omni_language=v1&audio_tts_language=v1
→ data: {
    audio_tts_language: [{code:"en-US", title:"English"}, …]   // 12 种
    omni_language:      [{code:"ar-SA", …}, …]                 // 30 种
    audio_tts_speakers: [{urls:[{language:"zh-CN", url:"https://download.qwen.ai/resources/tts/tibmres3/Cherry_zh.mp3"}, …]}, …]  // 50 个
    omni_speakers:      [{urls:[…]}, …]                        // 54 个
  }
```

只有**音色清单 + 试听 mp3**（静态 CDN），**没有任何合成端点参数**（无 model / 无 endpoint / 无 token）。

### 4.9.2 语音通话：通道在 HTTP 层之外

页面入口链：`选择模式 → 语音通话 / 视频通话`。点进去后 UI 正常（计时器 00:08/10:00、
蓝色声波球、静音/扬声器按钮），但：

| 观测手段 | 结果 |
|---|---|
| `fetch` hook（在点击前注入） | **0** 条相关请求 |
| `WebSocket` 构造函数替换 | **0** 次调用 |
| `getUserMedia` hook | **0** 次调用 |
| CDP `debug requests`（40 条全量） | **0** 条 ws / tts / audio / omni 请求 |

⇒ 通话通道不经过主线程的 fetch/WebSocket（典型形态是 WebRTC：媒体走 UDP，
信令面或无、或在页面加载期已建连；音频采集也可能在 worker/worklet 里）。

**结论：TTS/实时语音不做。** 理由不是"难"，而是：
- 没有 HTTP 合成端点可代理 —— 要做得先实现 WebRTC/WS 客户端 + Opus 编解码，
  那已经不是"网关"，是"另一个客户端"；
- 价值与其工程量不匹配（调用方要的通常是文本/图片/文件，不是实时通话）。

❌ 不要用 `tts/config` 里的 speaker 参数去猜 `/api/v2/tts/speech` 之类的路径 —— 该端点不存在，
   猜出来也只能是 404 或空响应。

## 4.10 积分（Credits）：第三个域名（✅ 2026-10-29 实测）

前面几节里上游是三块：`chat.qwen.ai`（对话/图片/视频/文件）、`auth.qwen.ai`（登录态）。
积分是**第四块**，而且很容易漏，因为它既不在用量页的 HTML 里、也不在 chat 域的 API 里。

### 4.10.1 怎么找到的（方法比结果有用）

先按常理猜了一圈 `/api/v2/credits/balance`、`/credits/summary`、`/credits/account` … 全 404。
后来换了做法：**让页面自己告诉我们**。

```js
// 在用量与账单页执行
performance.getEntriesByType('resource')
  .filter(e => e.initiatorType === 'xmlhttprequest')
  .map(e => e.name)
```

结果里出现两个新域名：

```
https://sg-member.qwen.com/api/member/level/query?biz_id=ai_qwen&…
https://sg-entitlement.qwen.com/api/entitlement/credits/credits/history?biz_id=ai_qwen&…&pageNum=1&pageSize=20
```

📌 两个之前踩过的坑：
- 页面用 **XHR**（不是 `fetch`）⇒ 之前 hook `window.fetch` 一条都没抓到，误判成"没有接口"；
- 余额**不是 SSR**（未水合的 HTML 里没有数字）⇒ 抓 HTML 也找不到。

### 4.10.2 端点与响应

```
GET https://sg-entitlement.qwen.com/api/entitlement/credits/credits/history
    ?biz_id=ai_qwen&chat_client=h5&device=pc&fr=pc&pr=qwen-i18n
    &la=zh-CN&tz=Asia%2FShanghai&wv=0.3.26&pageNum=1&pageSize=20
```

信封和 chat 域**不一样**（chat 是 `{success, request_id, data}`）：

```json
{"traceId":"…","httpCode":200,"success":true,"errorCode":null,"errorMsg":null,
 "data":{"totalCredits":280,"subscriptionCredits":270,"bonusCredits":10,
         "pageNum":1,"pageSize":20,"total":32,
         "usageList":[{"id":"217970435829026816",
           "title":"credits.dec.HappyHorse1.1.720P.5s.video",
           "date":1793209700000,"unit":"Credits","amount":30,"state":"-"}]}}
```

- `totalCredits` = 可用总额（= 订阅 + 奖励），**判够不够只看这个**；
- `usageList[]` 是流水：`amount` 正数入账、`state:"-"` 表示扣费；`title` 里带模型与档位；
- `biz_id` 少了会 404；`pageSize=1` 只取余额时返回体很小，适合高频轮询（本仓 60s 一次）。

### 4.10.2.1 每日赠送的时间点（✅ 2026-10-30 实测）

响应里**没有**"下次重置时间"字段，只有流水。每日赠送在 `usageList` 里表现为：

```json
{"title": "credits.init.title.daily", "amount": 40, "state": "+", "date": 1793324947000}
```

实测两批账号的这条记录时间戳（**注意：两批不同**）：

```
10-30 09:49 导入的 9 个号（main / a1~a5）
  main   09:49:18  (+40)
  a1     09:49:07  (+40)
  a2~a5  09:49:17  (+40)

10-30 14:00 导入的 3128 个号（抽样 11 个）
  9sivlv3ui / r6u0egxmy / …  14:01:24 ~ 14:01:27  (+40)
```

⇒ 发放时刻与**账号批次/首次激活时刻**高度相关（每批内部相差几秒，跨批相差数小时），
即按账号各自的 **24h 周期滚动**，**不是全平台统一时刻**。
（先前只看了上午那 6 个号，误判成"统一批处理"——已订正。）

普号每日 +40；会员（main）另有订阅额度，daily 同样 +40。

网关口径（不做假装精确）：
- `credits` 请求的 `pageSize` 从 1 改成 10，只为顺带捞出最近一条 daily；
- `Credits.LastDailyAt`（Unix 秒）由流水推导（`deriveDailyAt`），面板显示
  「上次入账」，并**按 +24h 估算**「预计下次」，悬停可见；
- 若上游哪天改成滚动发放，这个推算会失真 —— 那时以面板显示的"上次"为准。

### 4.10.3 计价表：`GET /api/v2/credits/pricing`（chat 域）

同一个概念在两个域各有一半，**必须两个都读**：

```json
{"success":true,"data":{
  "user":{"is_member":true,"member_type":"lite","member_expire_at":{…}},
  "image_generation":{"credits":1,"user_type":"all"},        // 出图 1 张 = 1 分
  "video_generation":[
    {"scene":"hh11T2v720P5s","desc":"视频生成-HappyHorse-高清-5s","consumeAmount":30,"kind":"all"},
    {"scene":"hhT2v720P5s",  "desc":"视频生成-HappyHorse-高清-5s","consumeAmount":40,"kind":"all"},
    … 共 132 条
  ]}}
```

🔴 **`scene` 前缀区分模型版本，价格差 33%~75%**：

| scene 前缀 | 模型 | 720P 5/10/15s | 1080P 5/10/15s |
|---|---|---|---|
| `hh11*` | HappyHorse **1.1**（本仓默认 `root_model=happyhorse11`） | **30 / 60 / 90** | **40 / 80 / 120** |
| `hh*` | HappyHorse 1.0 | 40 / 80 / 120 | 70 / 140 / 210 |
| `wan27*` / `wan25Preview*` / `wan30*` | Wan 系列 | 20~45 | 25~180 |

这条**踩过一次**：先按账单里的 `HappyHorse1.1.720P.5s = -30` 推了整张表，
再从 pricing 里读到 `hhT2v720P5s = 40`，一度以为账单和接口矛盾 ——
其实是我拿 `hh`（1.0）的行去对 `hh11`（1.1）的单。**按 scene 前缀取，别按 desc 取**
（desc 里两版都写"HappyHorse"，没有版本号）。

`kind` 字段：`all` = 任何额度可用；`purchase` = 需付费额度（1.0 的 1080P 全是这个）。
本仓不做区分，统一按"余额 ≥ 单价"判断 —— 余额不足时上游会给明确文案（见 §4.5.4）。

### 4.10.4 第三个"伪装成正常回复"的地方

积分不足时**提交视频仍返回 HTTP 200 + `success:true`**，只是没有 `task_id`：

```json
{"success":true,"data":{"messages":[{"content":
  "Credits 不足\n本次生成需要 30 Credits，当前可用 10 Credits。开通 Qwen 会员…"}]}}
```

本仓识别它（`isVideoCreditShortage`）并归类为 `KindQuota` ⇒ 对外 **429 insufficient_quota**。

> 至此"伪装成正常回复"共三处，都要单独识别：
> ① 图片流 `extra.update_member`（§4.5.4）；② 视频提交 `messages[0].content`（本节）；
> ③ 余额本身要主动查（本节 4.10.2），不查就永远不知道还剩多少。

## 5. 模型清单（✅ 2026-10-28）

`GET /api/v2/models/` 响应：

```json
{"success": true, "request_id": "…", "data": {"data": [ { "id": "…", "name": "…", "info": {…} } ]}}
```

实测 7 个模型（全部 `is_active: true`）：

| id | 思考 | 视觉 | 上下文 | 访客可用 |
|---|---|---|---|---|
| `qwen3.7-plus` | ✅ | ✅ | 1,000,000 | ✅ |
| `qwen3.8-max` | ✅ | ✅ | 1,000,000 | ✅ |
| `qwen3.8-omni-flash` | ✅ | ✅ | 1,000,000 | ❌ |
| `qwen3.7-max` | ✅ | ❌（描述明确"不支持视觉"） | 1,000,000 | ✅ |
| `qwen3.6-plus` | ✅ | ✅ | 1,000,000 | ✅ |
| `qwen3.5-plus` | ✅ | ✅ | 1,000,000 | ✅ |
| `qwen3.5-omni-plus` | ❌ | ✅ | 262,144 | ❌ |

- 注册口径：`info.is_active != false` 且 `info.meta.chat_type` 含 `t2t`。
- 匿名访问该端点会吃到 WAF（HTTP 405 的拦截页）⇒ **必须带凭据**。

### 5.1 没有独立的图片/视频模型 id（🔴 2026-10-30 实测）

这 7 个模型**每一个的 `chat_type` 都同时含 `t2t` / `t2i` / `t2v`**（个别还有
`image_edit` / `search` / `slides`…）。也就是说上游**不存在** `qwen-image-3.0`、
`happyhorse11` 这样的模型条目 —— 它们是"内核"而不是"模型"：

| 概念 | 藏在哪 | 取值（实测） |
|---|---|---|
| 图片内核 | `extra.meta.model` | `qwen-image-3.0` |
| 图片编辑 | `chat_type` / `sub_chat_type` | `image_edit` |
| 视频内核 | `messages[0].extra.meta.biz_data` → `req.rootModel` | `happyhorse11` |

所以：

- 把 `qwen-image-3.0` 填进 `chats/new` 的 `models[]` 会 **Bad_Request**；
- 前端 UI 的模型下拉框（`Qwen-Image 3.0` / `HappyHorse 1.1`）是**客户端自己的文案**，
  与 `models[]` 里的 chat 模型是两套东西；
- 本仓在 `/v1/models` 里把这两个内核包装成**生成模型条目**（`media: image|video`
  + `endpoint`），请求进来后再翻译成上面两个字段。翻译逻辑见
  `internal/server/mediamodels.go`。

---

## 6. 附件（✅ 已实现：图片 / 文件共用一条上传链）

上游 `files[]` 需要它自己 OSS 的 file 对象（`getstsToken` → V4 签名 PUT，签名 300s）。
外链 URL 会被拒 ⇒ 网关先把字节传上去，再带条目提交。

| 入口 | 落地方式 |
|---|---|
| `POST /v1/images/generations` 的 `image`（图生图） | `chat_type=image_edit` + `files[]`（**url 直通可用**，✅ 2026-10-30 实测） |
| `POST /v1/files` → content 里引用 `file_id` | `t2t` + `files[]`（`file_class=document`，**必须真 OSS 条目**，url 不行） |
| chat / messages / responses 的图片段 | `t2t` + `files[]`（`file_class=vision`，**走 url 直通/本地托管**） |

chat 的图片段三种写法全收（`image_url` / `input_image` / Anthropic `image.source`），
引用形态三收（URL / data URI / 裸 base64）。

### 6.1 🔴 `getstsToken` 已被阿里云 WAF 人机验证挡住（✅ 2026-10-30 实测）

```
POST /api/v2/files/getstsToken
→ HTTP 200 + <!doctype html> … <meta name="aliyun_waf_aa" content="…">
```

同一出口、同一时刻，`GET /api/v2/models/` 正常 200；14 个不同代理出口 + 直连
全部被挑战；`Bearer` / `Cookie: token=` / `Cookie: token+refresh_token` /
带或不带 `bx-ua` / 预热拿 `acw_tc`+`x-ap` —— 9 种组合无一通过。
**结论：端点级人机验证（滑块），不是凭据、不是 IP、不是头。**

**绕法：`files[].url` 直接给公网外链。** 三条链路的实测结论（2026-10-30，逐条端到端）：

| 用途 | `files[].url` 外链 | 证据 |
|---|---|---|
| vision（看图） | ✅ 可行 | 红图答「红色」；带字图答 `HELLO42` |
| image_edit（图生图） | ✅ 可行 | 白底黑字 `HELLO42` + 「背景改蓝」⇒ 2752×1376 蓝底黑字（宽高比同原图 2:1） |
| document（文件解析） | ❌ 不行 | 模型回「我无法直接读取您上传的文件内容」—— 服务端是按 `file_id` 去 OSS 取，不看 url |

所以：图片与图生图走「URL 直通 / 网关自托管 blob」（见 README）；
`POST /v1/files` 仍需要真 OSS 条目 ⇒ **当前受 §6.1 影响，会失败**。

⚠️ 若上游将来给 `url` 也加白名单，则 vision / image_edit 也要回到 STS（那时得先过滑块）。

---

## 7. 真实链路状态

| 步骤 | 结果 |
|---|---|
| 建会话 | ✅ 200，`data.id` 正常 |
| 流式对话（含思考） | ✅ 首字 ~1s，7 帧完整收流，usage 真实 |
| 模型清单 | ✅ 7 个模型 |
| 网关端到端 | 见 README「真实链路」一节 |

---

## 8. 未证实项（不得在实现里假设）

- **非流式上游**（`stream:false`）：未测。本仓恒走流式、本地聚合，不依赖它。
- **token 真实寿命**：`auths/refresh` 存在说明会过期，但具体时长未观测；本仓靠 401 被动失效 + 冷却兜底。
- **多会话并发上限**：未测。
- **附件上传链**：✅ 已实现（STS 签名 PUT），图生图 / 文件 / chat 图片三处共用。
- **风控阈值**：未观测到 RGV587（本仓按 429 软冷却处理）。

---

## 9. 与上一版公开契约的差异（2026-09-24 → 2026-10-28）

| 维度 | 旧契约（rsfree/qwen） | 本次实测 |
|---|---|---|
| 鉴权 | `Cookie: token=<JWT>` | **`Authorization: Bearer`**（cookie 基本空） |
| 鉴权域 | 与业务同域 | **`auth.qwen.ai` 独立** + `/auths/refresh` |
| `version` 头 | `0.2.0` | **`0.3.26`** |
| 模型清单 | `GET /api/models`，`data` 即数组 | **`GET /api/v2/models/`**，`data.data` 嵌套 |
| `parentId` | `""` | **上一条消息 id**（多轮链式） |
| 首帧 | 无 | **`response.created`** |
| usage | 顶层 | 顶层 + **`output_tokens_details.reasoning_tokens`** |
| 思考摘要 | `delta.extra` | 同（累加数组，需 diff） |
| 结束判据 | `finished` + `phase=answer` | 同 |

**结论**：请求体骨架（`chatId` 双写、`files: []`、`id: null`、`timestamp` 秒）**仍然有效**，
变的集中在**鉴权方式、版本号、模型端点、parent 链**四处。
