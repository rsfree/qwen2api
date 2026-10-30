package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// errNoAccount 池里没有可用账号。
var errNoAccount = errors.New("当前没有可用账号：全部处于冷却/失效状态，或未配置账号")

// errVideoQuotaLocal 本地每日视频配额（配置的每日预算）用尽。
var errVideoQuotaLocal = errors.New("本地每日视频配额已用尽")

// errCreditsShort 账号积分余额不足（上游账本口径，不是我们设的预算）。
//
// 与 errVideoQuotaLocal 分开是因为**处置完全不同**：
//   - 预算用尽 ⇒ 今天就是不能做了，改配置或等明天；
//   - 余额不足 ⇒ 可以充值，或换一个有余额的号，或等每日赠送。
//
// 两个都映射 429/insufficient_quota，但文案不能混 —— 混了就会让人去改一个
// 根本不生效的配置项（2026-10-29 实测踩到：报了"本地配额用尽"其实该充值）。
var errCreditsShort = errors.New("账号积分余额不足")

// errImageQuotaLocal 本地每日出图配额用尽（网关自己的限制，不是上游报错）。
//
// 与 upstream.ErrQuota 的区别：那个是**上游**说没额度了；
// 这个是**我们**主动不去撞上游硬限（打到硬限只会静默挂起 180s，体验更差）。
var errImageQuotaLocal = errors.New("本地每日出图配额已用尽")

// opened 一次成功建立的上游会话。
type opened struct {
	acct   *pool.Account
	stream *upstream.Stream
	chatID string
	// chatHit / chatMS 建会话的来源与耗时（流水观测：命中省掉一次 chats/new）。
	chatHit bool
	chatMS  int64
}

// handleChat `POST /v1/chat/completions`。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{
		ID:        "req-" + upstream.UUID()[:8],
		Path:      "/v1/chat/completions",
		ClientIP:  clientIP(r),
		UserAgent: truncateUA(r.UserAgent()),
	}

	var req ChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = "解析请求体: " + err.Error()
		s.addReq(&entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		entry.Status = http.StatusBadRequest
		entry.Error = "messages 为空"
		s.addReq(&entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	flat, err := Flatten(req.Messages)
	if err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = err.Error()
		s.addReq(&entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// 图片：content part 里的 image_url / input_image / image → 上游 files[]
	// （file_class=vision，见 docs/UPSTREAM.md §4.7）。
	// 引用三形态全收：URL / data URI / 裸 base64（与 /v1/images 的 image 字段同一条解链）。
	// 🔴 这里只解析引用、不下载 —— 上传必须在选号之后（file 是账号维度资源）。
	imgRefs, imgErr := collectImageRefs(req.Messages)
	if imgErr != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = imgErr.Error()
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", imgErr.Error(), "image")
		return
	}
	if len(imgRefs) > maxInputImages {
		entry.Status = http.StatusBadRequest
		entry.Error = "图片过多"
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("图片最多 %d 张（本次 %d 张）", maxInputImages, len(imgRefs)), "image")
		return
	}

	// 附件：content part 里的 file_id → 上游 files[] 条目（账号绑定的暂存）。
	// 纯文本附件走另一条：内容直接拼进 prompt（见 appendTextAttachments）。
	files, textFiles, filesErr := s.collectFiles(req.Messages)
	if filesErr != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = filesErr.Error()
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", filesErr.Error(), "file_id")
		return
	}
	prompt := flat.Prompt
	if len(textFiles) > 0 {
		prompt = appendTextAttachments(prompt, textFiles)
		flat.Degraded = joinDegraded(flat.Degraded,
			fmt.Sprintf("%d 个文本附件已内联进 prompt", len(textFiles)))
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.cfg.DefaultModel
	}
	// 清单里同时列了图片/视频模型（见 mediaModels）。拿它们调 chat 时给一句
	// 能指导处置的话 —— 不拦的话模型名会被甩给上游，换回一个 Bad_Request，
	// 调用方只会以为"网关坏了"。
	if ep := s.mediaEndpoint(model); ep != "" {
		entry.Model = model
		entry.Status = http.StatusBadRequest
		entry.Error = "生成模型不能用于 chat: " + model
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("模型 %s 是生成模型，不能用于 chat；请改调 %s", model, ep), "model")
		return
	}
	gear := req.ResolveThinking(s.cfg.DefaultThinking)
	entry.Model = model
	entry.Stream = req.Stream
	entry.Degraded = flat.Degraded

	// 文件解析仍走 t2t（2026-10-28 实测：t2t + files[] 就能读文件内容）。
	spec := upstream.TextSpec()
	spec.Files = files
	spec.ImageRefs = imgRefs
	sd, err := s.openUpstreamSpec(r.Context(), s.blobBase(r), model, gear, prompt, spec)
	if sd != nil {
		entry.ChatHit = sd.chatHit
		entry.ChatMS = sd.chatMS
	}
	if err != nil {
		entry.Account = accountOfErr(err)
		entry.Status = httpStatusFor(err)
		entry.Error = err.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(&entry)
		writeError(w, entry.Status, errTypeFor(err), err.Error())
		return
	}
	defer sd.stream.Close()
	defer s.pool.Release(sd.acct)
	entry.Account = sd.acct.Name

	if req.Stream {
		s.serveChatStream(w, r, sd, req, model, &entry, start)
	} else {
		s.serveChatOnce(w, r, sd, req, model, &entry, start)
	}
	s.pool.MarkOK(sd.acct)
}

// openUpstream 选号 → 建会话 → 开流，失败按可重试性换号。
//
// blobBase 是图片托管的公网基址（无图片时可为空）。
//
// 🔴 换号必须发生在**写响应之前**：一旦开始向客户端写 SSE，就再也回不了
// 正经 HTTP 状态，只能落成流内错误。
func (s *Server) openUpstream(ctx context.Context, blobBase, model, gear, prompt string,
	spec upstream.ChatSpec) (*opened, error) {
	return s.openUpstreamSpec(ctx, blobBase, model, gear, prompt, spec)
}

// openUpstreamSpec 同上，带类型规格（t2i 的图片模型 / t2v 的 biz_data 在 spec 里）。
func (s *Server) openUpstreamSpec(ctx context.Context, blobBase, model, gear, prompt string,
	spec upstream.ChatSpec) (*opened, error) {
	var lastErr error
	lastAcct := ""
	for attempt := 0; attempt < s.cfg.Pool.MaxRotate; attempt++ {
		acct, ok := s.pool.Pick()
		if !ok {
			// 🔴 这里也要带上"最后尝试的号"：只有 1 个号时，第二轮 Pick 就失败，
			// 走这条分支返回的裸 lastErr 会让流水 acct= 空（2026-10-30 抓到）。
			return nil, openFail(lastAcct, lastErr)
		}
		lastAcct = acct.Name
		attemptStart := time.Now()
		// 选号即触发出口探测（异步）：一个聊天请求要 5~15s、出图 60s+，
		// 等流水落库时探测早回来了 —— 这样"第一条请求"也能看到出口 IP。
		s.egressOf(acct.Name)
		// 凭据从池里取实时值（可能刚被自动续期改过），不要缓存 acct.Token。
		cred := s.pool.Credential(acct.Name)
		// 出口跟着账号走：带上账号标记，代理路由据此选固定 IP。
		actx := upstream.WithAccount(ctx, acct.Name)

		// 内层循环只做一件事：凭据过期时**续期后重试同一个号**。
		// 🔴 不能像以前那样 continue 到外层：外层是"换号"，会把重登一次
		// 当成一次换号 —— 3000 个冷号时，一次请求连登 4 个号就把预算用光，
		// 直接返回 503「没有可用账号」（2026-10-30 实测踩到）。
		var err error
		for retried := false; ; retried = true {
			var o *opened
			o, err = s.openOnce(actx, acct, cred, model, gear, prompt, blobBase, spec)
			if err == nil {
				o.acct = acct
				return o, nil
			}
			// 只在"凭据失效 + 还没重试过 + 有密码或 refresh_token"时续期一次。
			renewable := acct.Password != "" || s.pool.RefreshToken(acct.Name) != ""
			if retried || !renewable || !s.renewableErr(acct, err) {
				break
			}
			if !s.relogin(ctx, acct) {
				break
			}
			cred = s.pool.Credential(acct.Name)
		}

		s.pool.Release(acct)
		s.noteFailure(acct, err)
		lastErr = err
		took := time.Since(attemptStart)
		retrySafe := upstream.RetrySafe(err)
		// 「可证明未受理」的两类都换号：语义上安全的（401/额度/429/风控），
		// 以及**快速失败**（几秒内就挂掉的上游错误 —— 连活都没干）。
		if !retrySafe && !fastRetryable(err, took) {
			break
		}
		if !retrySafe {
			log.Printf("[qwen2api] 快速失败（%s，%s）：换号重试", acct.Name, took.Round(time.Millisecond))
		}
	}
	return nil, openFail(lastAcct, lastErr)
}

// openFail 统一包装"选号/开流失败"，带上最后尝试的账号名。
//
// 为什么要包：失败分支的流水必须能回答"是哪个号、从哪个出口"——
// acct= 空的 502 等于没有线索（2026-10-30 线上踩到两次）。
func openFail(lastAcct string, err error) error {
	if err == nil {
		return errNoAccount
	}
	return &errWithAccount{acct: lastAcct, err: err}
}

// takeChat 拿一个会话 id：优先用预建池里的，池空则现建（并顺手补一个备用）。
//
// 为什么值得（2026-10-30 实测）：`chats/new` 走住宅代理 2.0~2.8 秒，
// 而一次对话 5~8 秒 —— 命中缓存等于把这三分之一直接省掉。
//
// 🔴 只能给同一个账号用：跨号提交会 CHAT_NOT_FOUND（"此对话已删除"）。
// 所以 acctName 是必传的，且池子按 (账号, 会话类型) 分组。
// 返回 (id, 是否命中预建池, 现建耗时, err)。耗时只用于流水观测。
func (s *Server) takeChat(ctx context.Context, acctName, cred, model, chatType string) (string, bool, int64, error) {
	// 只有 t2t 值得维护池子（占请求绝大多数）；t2i / image_edit / t2v 一次
	// 就要 50~90 秒，建会话那 2.4s 占比可以忽略，补了纯属白花上游调用。
	refill := chatType == "t2t" && s.chats.Short(acctName, chatType) > 0
	if id, ok := s.chats.Take(acctName, chatType); ok {
		// 取出即删 ⇒ 立刻异步补一个，保持"下一个请求也能命中"。
		if refill {
			go s.prewarmChat(ctx, acctName, cred, model, chatType)
		}
		return id, true, 0, nil
	}
	// 记下现建的真实耗时：这是"命中能省多少"的唯一可信来源
	// （独立 curl 测的 2.4s 含 TLS 握手，网关是长连接）。
	t0 := time.Now()
	id, err := s.up.NewChatTyped(ctx, cred, model, chatType)
	took := time.Since(t0)
	if err == nil {
		s.chats.RecordBuild(took)
	}
	if err != nil {
		return "", false, took.Milliseconds(), err
	}
	if refill {
		go s.prewarmChat(ctx, acctName, cred, model, chatType)
	}
	return id, false, took.Milliseconds(), nil
}

// prewarmChat 后台建一个空会话放进池子。
//
// 失败静默丢弃：预热是"锦上添花"，任何错误都不该影响请求（真正的错误会在
// 请求路径上由 takeChat 的现建分支暴露出来）。
func (s *Server) prewarmChat(ctx context.Context, acctName, cred, model, chatType string) {
	if !s.chats.Enabled() {
		return
	}
	// 请求结束时 ctx 会取消，预热要活过请求 —— 但不能无界，
	// 用 WithoutCancel + 30s 上限，避免 goroutine 挂死。
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	id, err := s.up.NewChatTyped(pctx, cred, model, chatType)
	if err != nil {
		return
	}
	s.chats.Put(acctName, chatType, id)
}

// openOnce 用指定账号跑一轮：上传图片（若有）→ 建会话 → 提交 → 开流。
//
// 🔴 图片上传必须在这里做，不能提到选号之前：file 条目是账号维度资源，
// 用 A 号传的图让 B 号提交，上游会判"无权访问该文件"（且是静默的坏结果）。
// 放在这里还有个附带好处：换号重试时会自动用新账号重传一遍。
func (s *Server) openOnce(actx context.Context, acct *pool.Account, cred, model, gear, prompt, blobBase string,
	spec upstream.ChatSpec) (*opened, error) {

	attempt := spec
	if len(spec.ImageRefs) > 0 {
		// 🔴 不再走 getstsToken（该端点被阿里云 WAF 全局挑战）：URL 直给上游，
		// data URI / 裸 base64 存本地 BlobStore 后给公网 URL。见 blobs.go。
		files, err := s.visionFiles(actx, blobBase, spec.ImageRefs)
		if err != nil {
			return nil, err
		}
		attempt.Files = mergeAttachments(spec.Files, files)
	}

	chatID, hit, chatMS, err := s.takeChat(actx, acct.Name, cred, model, attempt.ChatType)
	if err != nil {
		return nil, err
	}
	body := upstream.BuildSubmitSpec(chatID, "", model, prompt, gear, attempt, time.Now())
	st, err := s.up.OpenChat(actx, cred, body)
	if err != nil {
		return nil, err
	}
	return &opened{stream: st, chatID: chatID, chatHit: hit, chatMS: chatMS}, nil
}

// visionFiles 把图片引用转成上游 files[] 条目。
//
// 两条来源：
//   - http(s) URL ⇒ **原样**交给上游（它自己去拉），零下载、零上传；
//   - data URI / 裸 base64 ⇒ 解码后存进本地 BlobStore，给一个我们自己的公网 URL。
//
// 为什么不做"先下载再上传 OSS"：getstsToken 已被 WAF 人机验证挡住（§6.1），
// 而 files[].url 本来就支持外链（实测模型能准确读出颜色与文字）。
func (s *Server) visionFiles(ctx context.Context, base string, refs []string) ([]upstream.FileRef, error) {
	files := make([]upstream.FileRef, 0, len(refs))
	for i, ref := range refs {
		entry, err := s.visionFile(ctx, base, ref)
		if err != nil {
			return nil, fmt.Errorf("第 %d 张参考图: %w", i+1, err)
		}
		files = append(files, entry)
	}
	return files, nil
}

// visionFile 单张：URL 直通 / base64 落 blob。
func (s *Server) visionFile(ctx context.Context, base, ref string) (upstream.FileRef, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		// 不下载：省一次出网，也避开"本机拉不到 / 防盗链"的坑。
		return upstream.VisionFileEntry(ref, "image"+extFromURL(ref), "image/png", 0), nil
	}
	data, mime, name, err := resolveImageRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if base == "" {
		return nil, errors.New("缺少公网基址：请配置 blob_base_url，或让反代带上 X-Forwarded-Proto/Host")
	}
	id, err := s.blobs.Put(data, mime)
	if err != nil {
		return nil, err
	}
	return upstream.VisionFileEntry(base+"/v1/blobs/"+id+extForMime(mime), name, mime, len(data)), nil
}

// extFromURL 从 URL 路径取扩展名（只认图片后缀，其余回 .png）。
func extFromURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.LastIndexByte(raw, '.'); i >= 0 && len(raw)-i <= 5 {
		switch strings.ToLower(raw[i:]) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
			return strings.ToLower(raw[i:])
		}
	}
	return ".png"
}

// blobBase 图片托管的公网基址：配置优先，否则用请求推断（反代场景）。
func (s *Server) blobBase(r *http.Request) string {
	if b := s.blobs.Base(); b != "" {
		return b
	}
	host := r.Host
	if host == "" {
		return ""
	}
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	return proto + "://" + host
}

// ensureFresh 确保账号的 access token 至少还剩 1 分钟寿命，否则先续期。
//
// 🔴 为什么上传前必须做（2026-10-30 实测）：`getstsToken` 对**过期的 Bearer
// 不返回 401**，而是 HTTP 200 + WAF 挑战页。于是：
//   · "401 才续期" 的兜底在图片上传路径上完全失效；
//   · 错误被分类成 KindRisk ⇒ 好号被软冷却（实测一次测试误伤 21 个号）。
// access TTL 只有 900s，而保活只热最前面几个号 ⇒ 池里绝大多数号的 token
// 长年处于"已过期"状态（实测 2863/3135），所以这不是边角情况而是常态。
func (s *Server) ensureFresh(ctx context.Context, acct *pool.Account, cred string) string {
	if s.tokenFresh(acct.Name, time.Minute) {
		return cred
	}
	if s.relogin(ctx, acct) {
		return s.pool.Credential(acct.Name)
	}
	log.Printf("[qwen2api] 账号 %s 上传前续期失败，仍按原凭据尝试（由上游判定）", acct.Name)
	return cred
}

// tokenFresh 凭据是否还剩 window 以上寿命（未知算不新鲜）。
func (s *Server) tokenFresh(name string, window time.Duration) bool {
	exp, ok := s.pool.TokenExpiry(name)
	return ok && time.Until(exp) > window
}

// renewableErr 这个错误是否值得"续期凭据后重试同一个号"。
//
// 除明确的 KindAuth 外，还要覆盖一种伪装：上传链（getstsToken）对过期 Bearer
// 返回 200 + WAF 挑战页 ⇒ 被分类成 KindRisk。若此时 token 确实已过期，
// 真实原因是凭据而不是风控 —— 不续期就会白白冷却一个好号。
func (s *Server) renewableErr(acct *pool.Account, err error) bool {
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		return true
	case upstream.KindRisk:
		return !s.tokenFresh(acct.Name, time.Minute)
	}
	return false
}

// mergeAttachments 拼 files[]：已暂存的附件（/v1/files 的 file_id）在前，本轮图片在后。
//
// 顺序不是装饰：上游把 files[] 当"用户这一条消息带的附件"，先后会影响模型
// 对"第几张图"的指代。附件身份（file-…）通常先被提及，所以排在前面。
func mergeAttachments(stored, uploaded []upstream.FileRef) []upstream.FileRef {
	if len(stored) == 0 {
		return uploaded
	}
	merged := make([]upstream.FileRef, 0, len(stored)+len(uploaded))
	merged = append(merged, stored...)
	merged = append(merged, uploaded...)
	return merged
}

// relogin 续期凭据：**优先用 refresh_token 换新 access**，失败才用密码重登。
//
// 为什么这个顺序很重要：access token 只有 15 分钟，而 signin 是整条链路里
// 最贵、最容易吃 WAF 的一步。refresh_token 换一张只要几百毫秒、不碰密码。
func (s *Server) relogin(ctx context.Context, acct *pool.Account) bool {
	// ① 有 refresh_token 就先走这条
	if rt := s.pool.RefreshToken(acct.Name); rt != "" {
		if access, next, err := s.up.RefreshWithToken(upstream.WithAccount(ctx, acct.Name), rt); err == nil {
			s.pool.SetToken(acct.Name, upstream.ModeBearer+":"+access)
			if next != "" {
				s.pool.SetRefresh(acct.Name, next)
			}
			log.Printf("[qwen2api] 账号 %s 凭据已续期（refresh_token）", acct.Name)
			return true
		} else {
			log.Printf("[qwen2api] 账号 %s refresh_token 续期失败，改用密码重登: %v", acct.Name, err)
		}
	}
	if acct.Email == "" || acct.Password == "" {
		return false
	}
	// 🔴 必须带账号上下文：登录要走**这个号的固定出口**。
	// 不带的话 SignIn 会从服务器真实 IP 直连上游 —— 与它平时的出口不一致，
	// 既容易吃 WAF（实测：3128 个号批量登录后，直连登录全被挑战页拦下），
	// 也正是风控画像里"账号登录地突变"的信号。
	access, refresh, err := s.up.SignInSession(upstream.WithAccount(ctx, acct.Name), acct.Email, acct.Password)
	if err != nil {
		log.Printf("[qwen2api] 账号 %s 自动重登失败: %v", acct.Name, err)
		return false
	}
	s.pool.SetToken(acct.Name, access)
	if refresh != "" {
		s.pool.SetRefresh(acct.Name, refresh)
	}
	log.Printf("[qwen2api] 账号 %s 凭据已自动续期（signin）", acct.Name)
	return true
}

// noteFailure 按错误分类处置账号。
func (s *Server) noteFailure(acct *pool.Account, err error) {
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		s.pool.MarkAuth(acct, err.Error())
	case upstream.KindQuota:
		s.pool.MarkQuota(acct, err.Error())
	case upstream.KindRateLimit, upstream.KindRisk:
		// 过期 Bearer 会被上游伪装成 WAF 页（getstsToken 实测），这类"风控"
		// 的真实原因是我们的凭据过期 —— 冷却只会白扔一个好号。
		if !s.tokenFresh(acct.Name, time.Minute) {
			log.Printf("[qwen2api] 账号 %s 报 %v，但 token 已过期 ⇒ 按凭据问题处理，不冷却",
				acct.Name, err)
			return
		}
		retry := 0
		var ue *upstream.Error
		if errors.As(err, &ue) {
			retry = ue.RetryAfter
		}
		s.pool.MarkRateLimit(acct, err.Error(), retry)
	default:
		// 其它错误不改账号状态（可能是模型名写错/请求体问题），只记日志。
	}
}

// ---------- 流式 ----------

func (s *Server) serveChatStream(w http.ResponseWriter, r *http.Request, sd *opened,
	req ChatRequest, model string, entry *reqlog.Entry, start time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		entry.Status = http.StatusInternalServerError
		entry.Error = "响应写入器不支持流式"
		s.addReq(entry)
		writeError(w, http.StatusInternalServerError, "internal_error", "响应写入器不支持流式")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	entry.Status = http.StatusOK
	id := "chatcmpl-" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	created := time.Now().Unix()

	// 首帧：只给 role，正文增量随后。
	s.writeChunk(w, flusher, ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []StreamChoice{{Index: 0, Delta: Delta{Role: "assistant"}}},
	})

	var (
		usage        *upstream.Usage
		chars        int
		reasoningLen int
		firstByte    bool
		sawUnknown   bool
		streamErr    error
	)
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			streamErr = err
			break
		}
		if !more {
			break
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.UnknownPhase != "" && !sawUnknown {
			sawUnknown = true
			log.Printf("[qwen2api] 上游出现未处理的 phase=%q（产物可能被丢弃，见 docs/UPSTREAM.md §4）", ev.UnknownPhase)
		}
		if ev.Reasoning != "" && s.cfg.ReasoningEnabled() {
			if !firstByte {
				entry.FirstByteMS = time.Since(start).Milliseconds()
				firstByte = true
			}
			reasoningLen += len([]rune(ev.Reasoning))
			s.writeChunk(w, flusher, ChatChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: Delta{ReasoningContent: ev.Reasoning}}},
			})
		}
		if ev.Text != "" {
			if !firstByte {
				entry.FirstByteMS = time.Since(start).Milliseconds()
				firstByte = true
			}
			chars += len([]rune(ev.Text))
			s.writeChunk(w, flusher, ChatChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: Delta{Content: ev.Text}}},
			})
		}
		if ev.Done {
			break
		}
	}

	// 收尾：流内错误要响亮地下发（不能静默当正常结束）。
	if streamErr != nil {
		entry.Error = streamErr.Error()
		if r.Context().Err() == nil {
			// 流内错误同样只给标准英文：详细原因留在 entry.Error。
			typ, msg := clientError(http.StatusBadGateway, errTypeFor(streamErr), streamErr.Error())
			s.writeSSE(w, flusher, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
		}
	} else if chars == 0 && reasoningLen == 0 {
		// 一字未出（含思考）：形态不符，必须让客户端知道，而不是回一个空回复。
		entry.Error = upstream.ErrNoText.Error()
		log.Printf("[qwen2api] 上游零文本，原始帧取证（%d 条）：%v", len(sd.stream.RawFrames()), sd.stream.RawFrames())
		s.writeSSE(w, flusher, ErrorBody{Error: ErrorDetail{
			Message: "upstream returned no content, please retry later",
			Type:    "upstream_error",
		}})
	}

	// 正常收尾（或错误后也发 finish，保证客户端能闭合）。
	stop := "stop"
	s.writeChunk(w, flusher, ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: &stop}},
	})
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage && usage != nil {
		s.writeChunk(w, flusher, ChatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []StreamChoice{}, Usage: toUsage(usage),
		})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	entry.Chars = chars
	if usage != nil {
		entry.PromptTokens = usage.InputTokens
		entry.OutputTokens = usage.OutputTokens
	}
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(entry)
}

// ---------- 非流式 ----------

func (s *Server) serveChatOnce(w http.ResponseWriter, r *http.Request, sd *opened,
	req ChatRequest, model string, entry *reqlog.Entry, start time.Time) {

	var (
		sb         strings.Builder
		reasoning  strings.Builder
		usage      *upstream.Usage
		sawUnknown bool
	)
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			// 还没写过响应头 ⇒ 可以回正经错误码。
			entry.Status = httpStatusFor(err)
			entry.Error = err.Error()
			entry.DurationMS = time.Since(start).Milliseconds()
			s.addReq(entry)
			writeError(w, entry.Status, errTypeFor(err), err.Error())
			return
		}
		if !more {
			break
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.UnknownPhase != "" && !sawUnknown {
			sawUnknown = true
			log.Printf("[qwen2api] 上游出现未处理的 phase=%q（产物可能被丢弃，见 docs/UPSTREAM.md §4）", ev.UnknownPhase)
		}
		if ev.Reasoning != "" && s.cfg.ReasoningEnabled() {
			reasoning.WriteString(ev.Reasoning)
		}
		if ev.Text != "" {
			sb.WriteString(ev.Text)
		}
		if ev.Done {
			break
		}
	}

	content := sb.String()
	if content == "" && reasoning.Len() == 0 {
		entry.Status = http.StatusBadGateway
		entry.Error = upstream.ErrNoText.Error()
		log.Printf("[qwen2api] 上游零文本，原始帧取证（%d 条）：%v", len(sd.stream.RawFrames()), sd.stream.RawFrames())
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(entry)
		writeError(w, http.StatusBadGateway, "upstream_error", upstream.ErrNoText.Error())
		return
	}

	stop := "stop"
	resp := ChatResponse{
		ID:      "chatcmpl-" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      &ResponseMessage{Role: "assistant", Content: content, ReasoningContent: reasoning.String()},
			FinishReason: &stop,
		}},
		Usage: toUsage(usage),
	}
	writeJSON(w, http.StatusOK, resp)

	entry.Status = http.StatusOK
	entry.Chars = len([]rune(content))
	entry.FirstByteMS = time.Since(start).Milliseconds()
	if usage != nil {
		entry.PromptTokens = usage.InputTokens
		entry.OutputTokens = usage.OutputTokens
	}
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(entry)
}

// writeChunk 写一个 OpenAI SSE 块。
func (s *Server) writeChunk(w http.ResponseWriter, f http.Flusher, c ChatChunk) {
	s.writeSSE(w, f, c)
}

func (s *Server) writeSSE(w http.ResponseWriter, f http.Flusher, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(raw)
	_, _ = io.WriteString(w, "\n\n")
	f.Flush()
}

// toUsage 上游 usage → OpenAI usage（字段名不同，别直接透传）。
func toUsage(u *upstream.Usage) *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if u.OutputTokensDetails.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens}
	}
	return out
}

// httpStatusFor 上游错误 → 对外 HTTP 状态码。
// fastFailWindow 一次尝试"多快失败"才说明上游没受理（快于它 ⇒ 换号安全）。
//
// 为什么按**时间**而不是只按错误类型：错误类型只能说明"哪种错"，说明不了
// "有没有受理"。一个 2 秒返回的 500/400 显然连活都没干；同样一个错拖到 78 秒
// 才回，就很可能已经受理并计费了 —— 那种要谨慎（超时另有"只试一次"的闸）。
const fastFailWindow = 10 * time.Second

// fastRetryable 快速失败的上游错误值得换号再试。
//
// 只看两条：是**上游错误**（*upstream.Error，排掉本地图片/文件解析错误）、
// 且用时 < fastFailWindow。实测这类"2~3 秒就挂"的 400/500 换个号立刻就好，
// 不重试等于把一次上游抖动直接甩给调用方。
func fastRetryable(err error, took time.Duration) bool {
	if err == nil || took >= fastFailWindow {
		return false
	}
	var ue *upstream.Error
	return errors.As(err, &ue)
}

func httpStatusFor(err error) int {
	if errors.Is(err, errNoAccount) {
		return http.StatusServiceUnavailable
	}
	// 上游"只发心跳不产出"是超时语义 ⇒ 504（调用方/SDK 一眼能区分 502 故障与超时）。
	if errors.Is(err, errUpstreamTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	// 账号不能用（未激活/封禁）：是"容量不足"，不是"你的凭据错了" ⇒ 503。
	if isAccountNotReady(err) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, upstream.ErrQuota) || errors.Is(err, errImageQuotaLocal) ||
		errors.Is(err, errVideoQuotaLocal) || errors.Is(err, errCreditsShort) {
		// 额度耗尽是"现在不行、等下/升级才行" ⇒ 429 而不是 502。
		return http.StatusTooManyRequests
	}
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		// 是网关的上游凭据坏了，不是调用方的 key 错了 ⇒ 502。
		return http.StatusBadGateway
	case upstream.KindQuota, upstream.KindRateLimit:
		return http.StatusTooManyRequests
	case upstream.KindBadInput:
		return http.StatusBadRequest
	case upstream.KindServer, upstream.KindTransport:
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

// errTypeFor 上游错误 → OpenAI error.type。
func errTypeFor(err error) string {
	if errors.Is(err, errNoAccount) {
		return "service_unavailable"
	}
	if errors.Is(err, upstream.ErrQuota) {
		return "insufficient_quota"
	}
	if errors.Is(err, errImageQuotaLocal) || errors.Is(err, errVideoQuotaLocal) ||
		errors.Is(err, errCreditsShort) {
		return "insufficient_quota"
	}
	if errors.Is(err, errUpstreamTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "upstream_timeout"
	}
	if isAccountNotReady(err) {
		return "service_unavailable"
	}
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		return "upstream_auth_error"
	case upstream.KindQuota:
		return "insufficient_quota"
	case upstream.KindRateLimit, upstream.KindRisk:
		return "rate_limit_error"
	case upstream.KindBadInput:
		return "invalid_request_error"
	}
	return "upstream_error"
}

// isAccountNotReady 上游明说"这个号还不能用"（见 upstream.CodeAccountNotReady）。
func isAccountNotReady(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && ue.Code == upstream.CodeAccountNotReady
}

// errWithAccount 给错误挂上"最后尝试的账号名"。
//
// 为什么不用字符串拼：流水要的是**结构化字段**（acct= 空就查不到号，
// 也带不出出口 IP）。保留 Unwrap，所以 KindOf / errors.Is 全部照旧。
type errWithAccount struct {
	acct string
	err  error
}

func (e *errWithAccount) Error() string { return e.err.Error() }
func (e *errWithAccount) Unwrap() error { return e.err }

// accountOfErr 取出错误里挂的账号名（没有则空）。
func accountOfErr(err error) string {
	var e *errWithAccount
	if errors.As(err, &e) {
		return e.acct
	}
	return ""
}

func truncateUA(ua string) string {
	if len(ua) > 120 {
		return ua[:120]
	}
	return ua
}

var _ = fmt.Sprintf
