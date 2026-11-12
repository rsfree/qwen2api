package server

// 视频前门：**火山方舟（Ark）原生协议**。
//
// 与文本/图片是两条读法：上游是**异步任务**——`POST completions`（stream:false）
// 同步回一个 `tsk_…` 任务 id，产物靠 `GET /api/v2/task/status/<id>` 轮询。
//
// 出口按方舟数据面的四个 surface 落（`references/protocol.md` §1）：
//
//	POST   /api/v3/contents/generations/tasks      → {"id":"cgt-…"}   ← 只有 id，受理 ≠ 成功
//	GET    /api/v3/contents/generations/tasks/{id} → 完整任务对象
//	GET    /api/v3/contents/generations/tasks      → {items[], total}
//	DELETE /api/v3/contents/generations/tasks/{id} → 200 + {"Result":{}}
//	GET    /api/v3/contents/generations/tasks/{id}/content
//	                                               → 成片字节（**网关扩展**，方舟无此端点）
//
// 🔴 五条不能省的纪律：
//  1. 创建回包只有 id。往里塞 status 就是承诺一个客户端不该依赖的形状。
//  2. `content.video_url` 只在 succeeded 时有值。未成功就交地址等于撒谎。
//  3. 任务 → 账号的映射必须落盘。上游任务活 7 天、产物 24h，
//     而 t2v 实测要 7 分钟才出 —— 内存 2h 撑不到终态就是契约破坏。
//  4. `usage` 的两个 token 字段承载**积分**（上游不统计 token）。语义借用，
//     必须在文档里写明，否则对账会把它当 token 读。
//  5. 方舟的 `queued` 可取消，本上游**没有取消端点** —— 非终态 DELETE 要明确拒绝，
//     不能假装取消成功（那会让调用方以为停了，实际还在跑并计费）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// videoTaskPath 方舟数据面的任务端点前缀（逐字等于原生，不加自己的段）。
const videoTaskPath = "/api/v3/contents/generations/tasks"

// videoPollMinInterval 同一个任务两次上游查询的最小间隔。
//
// 短视频客户端会 1s 一次地打，而上游查询 QPS 只有 20（账号维度）。
// 2 秒节流既不让客户端空等，也不会被自己的轮询打爆。
const videoPollMinInterval = 2

// videoWatchTick 后台巡检间隔（只处理带 callback_url 或未终态的任务）。
const videoWatchTick = 5 * time.Second

// videoCallbackTimeout 单次回调推送超时（skill §2.4：5 秒未确认最多重试 3 次）。
const videoCallbackTimeout = 5 * time.Second

// videoCallbackMaxTries 同状态最多推几次。
const videoCallbackMaxTries = 3

// ---------- 创建 ----------

// handleVideoCreate `POST /api/v3/contents/generations/tasks`。
func (s *Server) handleVideoCreate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{
		ID: "vid-" + upstream.UUID()[:8], Path: videoTaskPath,
		ClientIP: clientIP(r), UserAgent: truncateUA(r.UserAgent()),
	}

	req, rej := decodeVideoRequest(r.Body)
	if rej != nil {
		s.rejectVideo(w, r, rej, &entry, start)
		return
	}
	plan, rej := s.validateVideoRequest(req)
	if rej != nil {
		s.rejectVideo(w, r, rej, &entry, start)
		return
	}
	s.createVideo(w, r, plan, &entry, start)
}

// createVideo 校验通过后的提交路径。
func (s *Server) createVideo(w http.ResponseWriter, r *http.Request, plan *videoPlan,
	entry *reqlog.Entry, start time.Time) {

	// 上游必须有值才能提交：调用方没说的，用部署配置补（并在回包里回显实际生效值）。
	if plan.Resolution == "" {
		plan.Resolution = strings.ToUpper(strings.TrimSpace(s.conf().Video.Resolution))
	}
	if plan.Ratio == "" {
		plan.Ratio = strings.TrimSpace(s.conf().Video.Size)
	}
	if plan.Duration == 0 {
		plan.Duration = s.conf().Video.Duration
	}
	plan.EchoResolution = canonicalResolution(plan.Resolution)
	plan.EchoRatio = plan.Ratio
	plan.EchoDuration = plan.Duration

	// 🔴 幂等：同一份意图在「未终态 + 1 小时内」重复提交，返回同一个任务。
	// POST 超时后重试 POST 会造出两个计费任务，而客户端只认得第二个。
	if rec, ok := s.videos.FindLive(plan.Fingerprint, time.Now()); ok {
		writeJSON(w, http.StatusOK, map[string]string{"id": rec.ID})
		entry.Model = rec.Model
		entry.Account = rec.Account
		entry.Status = http.StatusOK
		entry.Error = "idempotent-reuse"
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(entry)
		return
	}

	// 积分：按 (resolution, duration) 档位算这一次要扣多少。
	// 查不到档位就明确拒绝 —— 上游只支持 5/10/15 秒，瞎填的时长本该在网关拦掉。
	needCredits, ok := s.conf().Video.VideoCredits(plan.Resolution, plan.Duration)
	if !ok {
		if len(s.conf().Video.Pricing) == 0 {
			needCredits = 0 // 未启用积分模式：退回个数口径
		} else {
			s.rejectVideo(w, r, rejectUnsupported("resolution",
				"不支持的档位 "+plan.Resolution+"/"+strconv.Itoa(plan.Duration)+"s；"+
					"支持的档位见 config.video.pricing（默认 720P|1080P × 5|10|15s）"),
				entry, start)
			return
		}
	}

	requested := plan.EchoModel
	// 复用校验阶段解析出的内核：两次调用之间配置可能热更，重新解析会有
	// 「校验按 happyhorse、提交按 wan27」的窗口，能力矩阵就对不上了。
	chatModel, _ := s.resolveVideoModel(requested)
	rootModel := plan.RootModel
	if rootModel == "" {
		_, rootModel = s.resolveVideoModel(requested)
	}

	var (
		acct    string
		taskID  string
		lastErr error
	)
	for attempt := 0; attempt < s.conf().Pool.MaxRotate; attempt++ {
		// 视频按积分计费，选号必须看「剩余预算够不够这一次」。
		a, ok := s.pool.PickForVideo(needCredits)
		if !ok {
			if lastErr != nil {
				break
			}
			// 分清「钱不够」与「号都不可用」：两者都让 Pick 失败，
			// 但调用方的处置完全不同（充值/等额度 vs 等冷却/加号）。
			if sent, msg, short := s.creditsShortfall(needCredits); short {
				lastErr = wrapShortfall(sent, msg)
				break
			}
			lastErr = errNoAccount
			break
		}
		s.egressOf(a.Name)
		// 余额是这个号「能不能做这一步」的唯一依据。选号后只刷它：
		// 池子 3000+ 号时全池刷新既慢又没意义。
		if _, known := s.pool.CreditBalance(a.Name); !known {
			s.refreshOneCredit(r.Context(), a.Name)
		}
		if bal, known := s.pool.CreditBalance(a.Name); known && needCredits > 0 && bal < needCredits {
			// 🔴 先 Release 再 continue：Pick 已把 inflight +1，
			// 漏掉这一步这个号会被永久算作「在途」，静默掉号。
			s.pool.Release(a)
			continue
		}

		attemptStart := time.Now()
		cred := s.pool.Credential(a.Name)
		actx := upstream.WithAccount(r.Context(), a.Name)

		chatID, _, _, err := s.takeChat(actx, a.Name, cred, chatModel, "t2v")
		if err == nil {
			spec, specErr := s.videoSpec(r, plan, rootModel)
			if specErr != nil {
				s.pool.Release(a)
				s.noteFailure(a, specErr)
				lastErr = specErr
				continue
			}
			body := upstream.BuildSubmitSpec(chatID, "", chatModel, plan.Prompt, "fast", spec, time.Now())
			taskID, err = s.up.SubmitVideo(actx, cred, body)
			if err == nil {
				acct = a.Name
				s.pool.MarkOK(a)
				s.pool.Release(a)
				break
			}
		}
		s.pool.Release(a)
		s.noteFailure(a, err)
		lastErr = err
		// 与聊天/生图同口径：语义未受理，或**快速失败**（提交只要几百毫秒，
		// 几秒就挂说明没建任务）都换号；慢失败不重试（提交可能已扣积分）。
		if !upstream.RetrySafe(err) && !fastRetryable(err, time.Since(attemptStart)) {
			break
		}
	}
	if taskID == "" {
		if lastErr == errNoAccount || lastErr == nil {
			if sent, msg, short := s.creditsShortfall(needCredits); short {
				lastErr = wrapShortfall(sent, msg)
			}
		}
		if lastErr == nil {
			lastErr = errNoAccount
		}
		entry.Status = httpStatusFor(lastErr)
		entry.Error = lastErr.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(entry)
		writeError(w, entry.Status, errTypeFor(lastErr), lastErr.Error())
		return
	}

	// 🔴 计数点在「拿到 task_id」：视频异步，提交即扣上游积分，
	// 产物要十几分钟后才知道 —— 等下载成功再计数就漏计了。
	s.pool.MarkVideoDone(s.pool.Get(acct))
	if needCredits > 0 {
		s.pool.SpendVideoCredits(s.pool.Get(acct), needCredits)
		s.pool.DebitCreditBalance(acct, needCredits)
	}

	now := time.Now()
	rec := &videoRecord{
		// 对外 id 用方舟格式 `cgt-…`，上游 `tsk_` 只留在 UpstreamID 里。
		ID: videoTaskID(now), UpstreamID: taskID,
		Account: acct, Model: plan.EchoModel, Fingerprint: plan.Fingerprint, Scene: plan.Scene,
		Resolution: plan.EchoResolution, Ratio: plan.EchoRatio,
		Duration: plan.EchoDuration, Audio: plan.EchoAudio,
		Status:    VideoStatusQueued,
		Credits:   needCredits,
		CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		ExpiresAt: now.Unix() + int64(plan.ExpiresAfter),
		CallbackURL: plan.CallbackURL,
	}
	s.videos.Put(rec)

	// 创建回包**只有 id**。受理 ≠ 成功，这是协议里最容易被「优化」坏的一条。
	writeJSON(w, http.StatusOK, map[string]string{"id": rec.ID})

	entry.Model = rec.Model
	entry.Account = acct
	entry.Status = http.StatusOK
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(entry)
}

// videoSpec 把执行计划翻成上游提交规格（含参考素材的 files[] 与 biz_data 扩展）。
//
// 必须在**选号之后**调用：file 条目是账号维度资源，用 A 号传的素材让 B 号提交，
// 上游会判「无权访问该文件」，而且是静默的坏结果。
//
// 🔴 载体最后决定（skill §3.7 第 3 条）：这一步才可能触发 IO（把 data URI
// 落成本地托管 URL），越晚绑定越好 —— 前面的校验与幂等判重都不该等下载。
func (s *Server) videoSpec(ctxReq *http.Request, plan *videoPlan, rootModel string) (upstream.ChatSpec, error) {
	extras := upstream.VideoExtras{GenMode: plan.GenMode, VideoMode: plan.VideoMode}
	var files []upstream.FileRef

	if len(plan.Clips) > 0 {
		base := s.blobBase(ctxReq)
		for i, clip := range plan.Clips {
			if clip.Kind != "image" {
				return upstream.ChatSpec{}, fmt.Errorf("第 %d 个参考素材：本上游只接受图片", i+1)
			}
			entry, err := s.visionFile(ctxReq.Context(), base, clip.Ref)
			if err != nil {
				return upstream.ChatSpec{}, fmt.Errorf("第 %d 张参考图: %w", i+1, err)
			}
			files = append(files, entry)
			// 🔴 attachments 的 materialId 取文件条目的 id（`img-<uuid>`）。
			// 这条链路**尚未实测** —— videoMaterialProbe 为 false 时根本走不到这里
			// （能力闸门会先拒）。实测确认后把开关默认值改成 true。
			if id, ok := entry["id"].(string); ok && id != "" {
				extras.Attachments = append(extras.Attachments, id)
			}
		}
	}

	spec := upstream.VideoSpecEx(plan.Ratio, plan.Resolution, plan.Duration, plan.Audio, rootModel, extras)
	spec.Files = files
	return spec, nil
}

// ---------- 查询 ----------

// handleVideoGet `GET /api/v3/contents/generations/tasks/{id}`。
func (s *Server) handleVideoGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, videoTaskPath+"/")
	if id == "" || strings.Contains(id, "/") {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", arkMissing, "", "缺少任务 id")
		return
	}
	rec, ok := s.videos.Get(id)
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "invalid_request_error", arkNotFound, "",
			"未知的任务 id（本地记录保留 8 天；上游任务 id 本身活 7 天）")
		return
	}
	// refresh=1 跳过 2 秒节流：客户端明确要最新状态时不该让它看缓存。
	force := r.URL.Query().Get("refresh") == "1"
	rec = s.refreshVideo(rec, force)
	writeJSON(w, http.StatusOK, videoTaskPayload(rec))
}

// handleVideoContent `GET /api/v3/contents/generations/tasks/{id}/content` —— 代理下载成片。
//
// ⚠️ **网关扩展**：方舟协议没有这个端点（它的 `content.video_url` 是 CDN 直链）。
// 加它是因为上游 CDN 的 WAF 会把 curl 类 UA 挡成 404，而方舟的 `video_url` 没有
// 「带浏览器 UA 的代取」入口。纯增量端点，不改变任何方舟契约。
//
// 为什么需要它：上游 CDN 的 WAF 只放行浏览器类 UA —— 客户端用 curl 默认 UA
// 直下会拿到 404（226 字节 HTML，和「还没转存完」无法区分）。
// 网关带浏览器 UA 代取，客户端就能像普通文件一样下载。
//
// Referer 不是必要条件：不带就 200，带外站值反而 403。
func (s *Server) handleVideoContent(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, videoTaskPath+"/"), "/content")
	rec, ok := s.videos.Get(id)
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "invalid_request_error", arkNotFound, "", "未知的任务 id")
		return
	}
	rec = s.refreshVideo(rec, false)
	if rec.Status != VideoStatusSucceeded || rec.VideoURL == "" {
		writeErrorCode(w, http.StatusConflict, "invalid_request_error", "not_ready", "",
			"任务还没有产物（status="+rec.Status+"）")
		return
	}
	data, err := fetchMedia(r.Context(), rec.VideoURL)
	if err != nil {
		writeErrorCode(w, http.StatusBadGateway, "upstream_error", "upstream_unreachable", "",
			"取回产物失败："+err.Error())
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", `inline; filename="`+id+`.mp4"`)
	_, _ = w.Write(data)
}

// handleVideoList `GET /api/v3/contents/generations/tasks`。
//
// 参数按方舟原生：`filter.model` / `filter.status` / `filter.task_ids`（可重复）
// / `page_num` / `page_size`。
//
// 只读本地快照，**不触发上游查询**：方舟的列表接口账号级 QPS 只有 1，
// 而我们要的状态在上游得一条条查 —— 拿列表当轮询入口会把池子打穿。
// 需要最新状态就查单个任务（`GET /{id}`，QPS 20）。
func (s *Server) handleVideoList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nums := func(key string, def, lo, hi int) int {
		n, err := strconv.Atoi(q.Get(key))
		if err != nil || n < lo {
			return def
		}
		if n > hi {
			return hi
		}
		return n
	}
	recs, total := s.videos.Query(videoQuery{
		Model:    strings.TrimSpace(q.Get("filter.model")),
		Status:   strings.TrimSpace(q.Get("filter.status")),
		IDs:      q["filter.task_ids"],
		PageNum:  nums("page_num", 1, 1, 500),
		PageSize: nums("page_size", 20, 1, 500),
	})
	items := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		items = append(items, videoTaskPayload(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": total,
	})
}

// handleVideoDelete `DELETE /api/v3/contents/generations/tasks/{id}`。
//
// 方舟的 DELETE 状态矩阵：`queued` 可取消 → `cancelled`；`running` 不支持；
// 终态删记录。**本上游一条都不满足**（qwen 网页端没有取消端点），所以：
//
//   - 非终态 ⇒ 400 `InvalidParameter.OperationDenied` 明确说「不支持取消」。
//     绝不假装取消成功 —— 那会让调用方以为任务停了，实际还在跑并继续计费。
//   - 终态 ⇒ 删本地记录（此后查不到），回方舟的 `{"Result":{}}`。
//
// 这是协议与上游能力的真实缺口，写进 README 的「已知偏差」，不靠猜。
func (s *Server) handleVideoDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, videoTaskPath+"/")
	rec, ok := s.videos.Get(id)
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "invalid_request_error", arkNotFound, "", "未知的任务 id")
		return
	}
	if !IsTerminalVideoStatus(rec.Status) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", arkDenied, "",
			"本上游没有取消端点：status="+rec.Status+" 的任务无法取消，只能等它终态"+
				"（方舟的 queued 可取消，这里做不到）")
		return
	}
	s.videos.Delete(id)
	writeJSON(w, http.StatusOK, map[string]any{"Result": map[string]any{}})
}

// handleVideoLegacy 旧路径（`/v1/videos*`）的下线应答。
//
// 视频出口统一到方舟原生路径，`/v1/videos` 与更早的 `/v1/videos/generations` 都下线。
// 410 是诚实的答复 —— 404 会让调用方以为打错了地址，一直重试；
// 而静默兼容会让两套形状（`url` 平铺 / `content.video_url`）并存，字段永远对不上。
func (s *Server) handleVideoLegacy(w http.ResponseWriter, r *http.Request) {
	writeErrorCode(w, http.StatusGone, "invalid_request_error", "EndpointRetired", "",
		"视频出口已改为火山方舟原生协议：POST/GET/DELETE "+videoTaskPath)
}

// ---------- 刷新 ----------

// refreshVideo 查一次上游并把结果写回记录。
//
// 三类错误分开处置（skill §2.5）：
//   - 观测期（GET 网络错 / 5xx）⇒ 保留上次状态，可重试，绝不重新 POST；
//   - 运行期 failed ⇒ 落 error，终态；
//   - 本地超时 ⇒ 标 expired，不再查上游（上游可能已经查不到了）。
func (s *Server) refreshVideo(rec *videoRecord, force bool) *videoRecord {
	now := time.Now()

	// 已经拿到产物的成功任务：不用再查（省上游 QPS），链接本身 24h 内有效。
	if rec.Status == VideoStatusSucceeded && rec.VideoURL != "" {
		return rec
	}
	// 后处理中：上游那一侧已经到顶了，再查没有新信息（重试由 polisher 负责）。
	if rec.Status == VideoStatusPolishing && rec.UpstreamURL != "" {
		return rec
	}
	if !force && now.Unix()-rec.PolledAt < videoPollMinInterval {
		return rec
	}
	// polishing 手里已有产物，不再受 execution_expires_after 约束 ——
	// 那个超时管的是"上游还出不出片"，不是"我们的后处理要多久"。
	if !IsTerminalVideoStatus(rec.Status) && rec.Status != VideoStatusPolishing &&
		now.Unix() >= rec.ExpiresAt {
		rec.Status = VideoStatusExpired
		rec.ErrCode = "expired"
		rec.ErrMsg = "任务超过 execution_expires_after（" +
			strconv.FormatInt(rec.ExpiresAt-rec.CreatedAt, 10) + "s）仍未终态"
		rec.UpdatedAt = now.Unix()
		rec.PolledAt = now.Unix()
		s.videos.Put(rec)
		return rec
	}

	cred := s.pool.Credential(rec.Account)
	if cred == "" {
		// 账号掉出池子：不改状态（这是网关侧问题，不是任务失败）。
		return rec
	}
	raw, err := s.up.TaskStatus(upstream.WithAccount(context.Background(), rec.Account), cred, rec.UpstreamID)
	if err != nil {
		// 观测期失败：状态与产物都不动，下次再试。
		// 🔴 必须 Put：Get 返回的是**副本**，只改副本等于没记节流时间 ——
		// 上游一挂，客户端每次轮询都会再打一次上游。
		rec.PolledAt = now.Unix()
		s.videos.Put(rec)
		return rec
	}

	prevStatus, prevURL := rec.Status, rec.VideoURL
	rawStatus := upstream.VideoStatusRaw(raw)
	status, known := upstream.NormalizeVideoStatus(rawStatus)
	video, lastFrame := upstream.VideoProduct(raw)
	code, msg := upstream.VideoFailure(raw)
	completion, total, hasUsage := upstream.VideoUsage(raw)

	// 🔴 终态保护：非终态不许覆盖终态。
	// 回调重推、迟到的 running 都会走到这里 —— 让 running 盖掉 succeeded
	// 等于把已经交付的产物重新变成「还没好」。
	if IsTerminalVideoStatus(rec.Status) && !IsTerminalVideoStatus(status) {
		return rec
	}
	if status == VideoStatusSucceeded && video == "" {
		// 上游说成功但没给地址：既不能交付也不能当失败 ⇒ 收敛为非终态等下一轮。
		status = VideoStatusRunning
	}

	rec.Status = status
	rec.StatusRaw = rawStatus
	rec.StatusUnknown = !known
	rec.PolledAt = now.Unix()
	if video != "" && rec.UpstreamURL == "" {
		// 上游直链只进内部字段。它是后处理失败重试的唯一素材来源，
		// 而这个链接 24h 后就 404 —— 必须第一时间存下来。
		rec.UpstreamURL = video
		rec.ProductAt = now.Unix()
	}
	// 产物到手：启用托管时转 polishing，对外绝不暴露带水印的直链。
	if status == VideoStatusSucceeded {
		if st := s.conf().Storage; st.Enabled && rec.UpstreamURL != "" {
			rec.Status = VideoStatusPolishing
			rec.PolishState = "queued"
			if s.polish != nil {
				s.polish.enqueue(rec.ID)
			}
		} else if rec.VideoURL == "" {
			rec.VideoURL = rec.UpstreamURL
		}
	}
	if lastFrame != "" {
		rec.LastFrame = lastFrame
	}
	if status == VideoStatusFailed {
		rec.ErrCode, rec.ErrMsg = code, msg
		if rec.ErrCode == "" {
			rec.ErrCode = "upstream_failed"
		}
		if rec.ErrMsg == "" {
			rec.ErrMsg = "上游任务失败（原始状态：" + rawStatus + "）"
		}
	}
	if hasUsage {
		rec.CompletionTokens, rec.TotalTokens = completion, total
	}
	// updated_at 只在**真的有变化**时前进：它是给客户端做「任务动过了」判断的，
	// 每次轮询都刷新会把这个信号淹掉。
	if rec.Status != prevStatus || rec.VideoURL != prevURL || rec.UpdatedAt == 0 {
		rec.UpdatedAt = now.Unix()
	}
	// 但记录一定要写回：即使状态没变，PolledAt / StatusRaw 也变了，
	// 而这两者正是节流与排障的依据（Get 返回的是副本）。
	s.videos.Put(rec)
	return rec
}

// ---------- 序列化（唯一一份） ----------

// videoTaskPayload canonical 任务对象。
//
// 🔴 查询、回调推送、列表共用这一个函数。
// 出现第二个序列化器，「回调体和查询体字段对不上」只是时间问题。
func videoTaskPayload(rec *videoRecord) map[string]any {
	// polishing 对外一律呈现为 running。
	// 🔴 内部进度不泄漏到协议层：方舟只有六态，自造一个 "polishing" 会让
	// 按状态机写死的客户端永远等不到终态。
	status := rec.Status
	if status == VideoStatusPolishing {
		status = VideoStatusRunning
	}
	out := map[string]any{
		"id":         rec.ID,
		"model":      rec.Model,
		"status":     status,
		"created_at": rec.CreatedAt,
		"updated_at": rec.UpdatedAt,
	}
	if rec.Resolution != "" {
		out["resolution"] = rec.Resolution
	}
	if rec.Ratio != "" {
		out["ratio"] = rec.Ratio
	}
	if rec.Duration > 0 {
		out["duration"] = rec.Duration
	}
	out["generate_audio"] = rec.Audio

	// content.video_url 仅 succeeded 有值；其余状态为 null。
	if rec.Status == VideoStatusSucceeded && rec.VideoURL != "" {
		content := map[string]any{"video_url": rec.VideoURL}
		if rec.LastFrame != "" {
			content["last_frame_url"] = rec.LastFrame
		} else {
			content["last_frame_url"] = nil
		}
		out["content"] = content
		out["error"] = nil
	} else {
		out["content"] = map[string]any{"video_url": nil}
		if rec.Status == VideoStatusFailed {
			code := rec.ErrCode
			if code == "" {
				code = "upstream_failed"
			}
			out["error"] = map[string]any{"code": code, "message": rec.ErrMsg}
		} else {
			out["error"] = nil
		}
	}

	// usage：方舟的 token 字段在本网关承载**积分**。
	//
	// Qwen 视频不统计 token（只扣积分），而方舟客户端普遍读 usage 做计费对账。
	// 三个选择：编一个 token 数（对账必然错）、省略（客户端当 0，以为免费）、
	// 把**真实积分**放进 token 字段。这里选第三个，并同时给 credits。
	//
	// 🔴 语义借用，文档必须写明：别拿 completion_tokens 反推帧数或时长，
	//    它是「这次扣了几个积分」，不是模型消耗的 token。
	if rec.Credits > 0 {
		usage := map[string]any{
			"completion_tokens": rec.Credits,
			"total_tokens":      rec.Credits,
			"credits":           rec.Credits,
		}
		// 上游将来若真给了 token，单独放一个键，别和积分混在同一字段里。
		if rec.CompletionTokens > 0 {
			usage["upstream_tokens"] = rec.CompletionTokens
		}
		out["usage"] = usage
	} else if rec.CompletionTokens > 0 {
		out["usage"] = map[string]any{
			"completion_tokens": rec.CompletionTokens,
			"total_tokens":      rec.TotalTokens,
		}
	}
	// 排查用扩展：上游状态原文 + 是否收敛过。
	if rec.StatusRaw != "" {
		out["status_raw"] = rec.StatusRaw
	}
	if rec.StatusUnknown {
		out["status_unnormalized"] = true
	}
	return out
}

// ---------- 后台巡检 + 回调推送 ----------

// videoWatchOnce 一轮巡检：刷新未终态任务，并推送回调。
func (s *Server) videoWatchOnce() {
	for _, rec := range s.videos.Pending() {
		before := rec.Status
		rec = s.refreshVideo(rec, false)
		if rec.CallbackURL == "" {
			continue
		}
		// 五态推送，不含 cancelled（主动取消不触发回调）。
		// queued 不推：创建不算「状态变更」。
		switch rec.Status {
		case VideoStatusQueued, VideoStatusCancelled:
			continue
		}
		if rec.CallbackState == rec.Status && rec.Status == before {
			continue
		}
		if rec.CallbackState == rec.Status {
			continue
		}
		s.pushVideoCallback(rec)
	}
}

// pushVideoCallback 推送一次；同状态最多 3 次。
func (s *Server) pushVideoCallback(rec *videoRecord) {
	body := videoTaskPayload(rec)
	if s.postCallback(rec.CallbackURL, body, videoCallbackTimeout) {
		rec.CallbackState = rec.Status
		rec.CallbackTries = 0
		s.videos.Put(rec)
		return
	}
	rec.CallbackTries++
	if rec.CallbackTries >= videoCallbackMaxTries {
		// 放弃：不再重推，但保留记录（运维能在面板里看到 callback_tries）。
		rec.CallbackState = rec.Status
	}
	s.videos.Put(rec)
}

// videoWatchLoop 常驻巡检。回调依赖它 —— 没有它，客户端不查询就永远收不到推送。
func (s *Server) videoWatchLoop(stop <-chan struct{}) {
	t := time.NewTicker(videoWatchTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.videoWatchOnce()
		}
	}
}

// ---------- 拒绝应答 ----------

// rejectVideo 把协议级拒绝写成四键信封。
//
// warnings 放响应头（URL 编码，保证 ASCII 可传）：canonical 的 error 信封
// additionalProperties=false，塞进去就是破坏契约；只写日志又等于调用方看不见。
func (s *Server) rejectVideo(w http.ResponseWriter, r *http.Request, rej *videoReject,
	entry *reqlog.Entry, start time.Time) {

	if rej == nil {
		rej = rejectInvalid("", "请求不合法")
	}
	if len(rej.Warnings) > 0 {
		w.Header().Set("X-Qwen2API-Warnings", url.QueryEscape(strings.Join(rej.Warnings, "\n")))
	}
	entry.Status = rej.Status
	entry.Error = rej.Message
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(entry)
	writeErrorCode(w, rej.Status, rej.Type, rej.Code, rej.Param, rej.Message)
}

// canonicalResolution 上游拼写 → canonical 拼写（720P → 720p）。
func canonicalResolution(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// wrapShortfall 把「积分不够」的错误包装成可归因的形态。
func wrapShortfall(sent error, msg string) error {
	if sent == nil {
		return errNoAccount
	}
	return &shortfallErr{sent: sent, msg: msg}
}

type shortfallErr struct {
	sent error
	msg  string
}

func (e *shortfallErr) Error() string { return e.msg }
func (e *shortfallErr) Unwrap() error { return e.sent }

// callbackClient 专用于回调推送。
//
// 🔴 不吃环境代理：callback_url 多半是调用方自己的内网服务，
// 走 HTTP_PROXY 会被代理**静默吞掉**（POST 尤其），而失败只表现为「回调没来」——
// 排障时没人会想到是网关的代理设置。
var callbackClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	},
}

// postCallback 推送一次 canonical 任务对象。
//
// 推送体与查询响应**逐键同构**（同一个 videoTaskPayload 产出）。
// 幂等在接收方：按 (id, status) 去重，终态不可被非终态覆盖。
func (s *Server) postCallback(target string, body map[string]any, timeout time.Duration) bool {
	raw, err := json.Marshal(body)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "qwen2api/video-callback")
	resp, err := callbackClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
