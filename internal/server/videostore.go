package server

// 视频任务映射的持久化。
//
// 🔴 为什么必须落盘（此前只存内存 2h，是一处真契约破坏）：
// 上游任务 ID 活 7 天、产物 24h，而一个视频要 10~15 分钟才出片。
// 内存 2h + 进程重启 ⇒ 客户端手里的 id 直接 404，只能重新提交 ——
// 而重新提交是**第二次计费**，用户还得再等一轮。
//
// 落盘粒度：整份 JSON（任务量级是千条以内，全量写比增量追加更不容易写坏）。
// 写盘节流 500ms，高频轮询不会把磁盘写爆；进程退出前 Flush 一次。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// videoRecordTTL 本地记录保留时长。上游任务 ID 活 7 天，本地多留一天做对账余量。
const videoRecordTTL = 8 * 24 * time.Hour

// videoSaveInterval 落盘节流间隔。
const videoSaveInterval = 500 * time.Millisecond

// videoRecord 一条任务的**全部**状态。查询 / 回调 / 列表都从这一份序列化，
// 不允许出现第二套字段映射（否则「回调体和查询体对不上」是必然的）。
type videoRecord struct {
	ID         string `json:"id"`
	UpstreamID string `json:"upstream_id"`
	Account    string `json:"account"`
	Model      string `json:"model"`
	// Fingerprint 幂等键：同一份意图重复提交命中同一个任务。
	Fingerprint string `json:"fingerprint,omitempty"`
	// Scene 折叠后的场景（t2v / first_frame / first_last / omni_ref），排查用。
	Scene string `json:"scene,omitempty"`

	// ---- 回显参数：创建时定下，此后只读 ----
	// Watermark 客户端是否要求成片带水印（方舟原生字段，默认 false）。
	// 决定后处理走向：false ⇒ 去水印；true ⇒ 原样交付。
	Watermark  bool   `json:"watermark,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Ratio      string `json:"ratio,omitempty"`
	Duration   int    `json:"duration,omitempty"`
	Audio      bool   `json:"audio,omitempty"`

	// ---- 运行态 ----
	Status string `json:"status"`
	// VideoURL 对外交付地址。启用 storage 时是自建存储的 URL（已去水印），
	// 否则是上游直链。
	VideoURL string `json:"video_url,omitempty"`
	// UpstreamURL 上游 CDN 直链（**内部字段，不对外**）。
	// 必须单独存：它是后处理失败重试的唯一素材来源，而它 24h 后就 404 了。
	UpstreamURL string `json:"upstream_url,omitempty"`
	// PolishState 去水印后处理进度："" | queued | running | done | failed。
	// 与 Status 分开是因为 Status 要遵守方舟六态（不能自造对外状态）。
	PolishState string `json:"polish_state,omitempty"`
	PolishTries int    `json:"polish_tries,omitempty"`
	PolishErr   string `json:"polish_err,omitempty"`
	LastFrame string `json:"last_frame_url,omitempty"`
	ErrCode   string `json:"error_code,omitempty"`
	ErrMsg    string `json:"error_message,omitempty"`
	// StatusRaw 上游状态原文：判错时至少能看见原始值。
	StatusRaw string `json:"status_raw,omitempty"`
	// StatusUnknown 上游给了个词表外的值，已按非终态收敛（skill §3.6）。
	StatusUnknown bool `json:"status_unknown,omitempty"`

	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// Credits 网关本次扣的积分。视频不统计 token，这是唯一的计费证据。
	Credits int `json:"credits,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
	// ExpiresAt 本地到期时刻（created_at + execution_expires_after）。
	ExpiresAt int64 `json:"expires_at"`
	// ProductAt 首次拿到产物的时刻 —— 产物 24h 寿命从这里起算。
	ProductAt int64 `json:"product_at,omitempty"`
	// PolledAt 上次上游查询时刻（节流用）。
	PolledAt int64 `json:"polled_at,omitempty"`

	CallbackURL string `json:"callback_url,omitempty"`
	// CallbackState 已推送过的状态：同状态不重推（幂等键是 id+status）。
	CallbackState string `json:"callback_state,omitempty"`
	CallbackTries int    `json:"callback_tries,omitempty"`
}

// videoStoreFile 落盘文件形态（带版本号，将来改结构能识别）。
type videoStoreFile struct {
	Version int            `json:"version"`
	Items   []*videoRecord `json:"items"`
}

const videoStoreVersion = 1

// videoStore 任务存储。path 为空 ⇒ 纯内存（测试 / 未配置落盘）。
type videoStore struct {
	mu    sync.Mutex
	path  string
	items map[string]*videoRecord
	byFP  map[string]string // fingerprint -> id

	dirty   bool
	saving  bool
	lastSave time.Time
}

// newVideoStore 构造并尝试加载历史记录。
func newVideoStore(path string) *videoStore {
	v := &videoStore{path: path, items: map[string]*videoRecord{}, byFP: map[string]string{}}
	if path == "" {
		return v
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[qwen2api] 读取视频任务记录失败（按空库启动）：%v", err)
		}
		return v
	}
	var f videoStoreFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// 坏文件不阻塞启动：视频是旁路能力，起不来比丢历史更糟。
		log.Printf("[qwen2api] 视频任务记录损坏（按空库启动）：%v", err)
		return v
	}
	cut := time.Now().Add(-videoRecordTTL).Unix()
	for _, rec := range f.Items {
		if rec == nil || rec.ID == "" || rec.CreatedAt < cut {
			continue
		}
		v.items[rec.ID] = rec
		if rec.Fingerprint != "" && !IsTerminalVideoStatus(rec.Status) {
			v.byFP[rec.Fingerprint] = rec.ID
		}
	}
	return v
}

// Path 落盘路径（空 = 纯内存）。
func (v *videoStore) Path() string { return v.path }

// Put 写入 / 覆盖一条记录。
func (v *videoStore) Put(rec *videoRecord) {
	v.mu.Lock()
	v.items[rec.ID] = rec
	if rec.Fingerprint != "" && !IsTerminalVideoStatus(rec.Status) {
		v.byFP[rec.Fingerprint] = rec.ID
	}
	v.dirty = true
	v.mu.Unlock()
	v.maybeSave()
}

// Get 取一条记录（返回**副本**：调用方改字段不会绕过 Put 造成漏盘）。
func (v *videoStore) Get(id string) (*videoRecord, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.items[id]
	if !ok {
		return nil, false
	}
	if time.Since(time.Unix(rec.CreatedAt, 0)) > videoRecordTTL {
		return nil, false
	}
	cp := *rec
	return &cp, true
}

// FindLive 按指纹找**还能复用**的任务。
//
// 只有未终态、未超本地超时的任务才算命中：命中终态任务意味着「同样意图再要一份」，
// 那是新任务，不是重试。1 小时的硬上限防的是「同一个人反复点生成」——
// 那时他要的确实是新的一条片子。
func (v *videoStore) FindLive(fp string, now time.Time) (*videoRecord, bool) {
	if fp == "" {
		return nil, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	id, ok := v.byFP[fp]
	if !ok {
		return nil, false
	}
	rec, ok := v.items[id]
	if !ok || IsTerminalVideoStatus(rec.Status) || now.Unix() >= rec.ExpiresAt {
		return nil, false
	}
	if now.Sub(time.Unix(rec.CreatedAt, 0)) > time.Hour {
		return nil, false
	}
	cp := *rec
	return &cp, true
}

// videoQuery 列表筛选（方舟 `filter.*` + 分页）。
//
// 方舟原生参数：
//
//	filter.model / filter.status / filter.task_ids（重复参数名）
//	page_num [1,500]（默认 1） / page_size [1,500]（默认 20）
//
// `filter.service_tier` 本网关不支持（上游没有服务等级概念）—— 不实现就忽略，
// 但要在文档声明：传了它不会过滤，而不是假装过滤了。
type videoQuery struct {
	Model    string
	Status   string
	IDs      []string
	PageNum  int
	PageSize int
}

// Query 按方舟参数筛选并分页，返回当前页与**过滤后的总数**。
func (v *videoStore) Query(q videoQuery) ([]*videoRecord, int) {
	v.mu.Lock()
	defer v.mu.Unlock()

	idSet := map[string]struct{}{}
	for _, id := range q.IDs {
		idSet[id] = struct{}{}
	}
	all := make([]*videoRecord, 0, len(v.items))
	for _, rec := range v.items {
		if q.Model != "" && rec.Model != q.Model {
			continue
		}
		if q.Status != "" && rec.Status != q.Status {
			continue
		}
		if len(idSet) > 0 {
			if _, ok := idSet[rec.ID]; !ok {
				continue
			}
		}
		cp := *rec
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt == all[j].CreatedAt {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt > all[j].CreatedAt
	})

	total := len(all)
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	pageNum := q.PageNum
	if pageNum <= 0 {
		pageNum = 1
	}
	start := (pageNum - 1) * pageSize
	if start >= total {
		return nil, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return all[start:end], total
}

// Count 记录总数（列表端点的 total 语义是「一共有多少条」，不是本页条数）。
func (v *videoStore) Count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.items)
}

// Delete 删本地记录（上游没有取消能力，这是「忘掉这条」而不是「取消任务」）。
func (v *videoStore) Delete(id string) bool {
	v.mu.Lock()
	rec, ok := v.items[id]
	if ok {
		delete(v.items, id)
		if rec.Fingerprint != "" && v.byFP[rec.Fingerprint] == id {
			delete(v.byFP, rec.Fingerprint)
		}
		v.dirty = true
	}
	v.mu.Unlock()
	if ok {
		v.maybeSave()
	}
	return ok
}

// Pending 需要后台关注的任务：未终态，或终态但回调还没确认。
func (v *videoStore) Pending() []*videoRecord {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*videoRecord, 0, 8)
	for _, rec := range v.items {
		if IsTerminalVideoStatus(rec.Status) && rec.CallbackState == rec.Status {
			continue
		}
		cp := *rec
		out = append(out, &cp)
	}
	return out
}

// Prune 清掉超过 TTL 的记录（启动后与每次落盘前顺手做）。
func (v *videoStore) Prune(now time.Time) int {
	cut := now.Add(-videoRecordTTL).Unix()
	v.mu.Lock()
	n := 0
	for id, rec := range v.items {
		if rec.CreatedAt < cut {
			delete(v.items, id)
			if rec.Fingerprint != "" && v.byFP[rec.Fingerprint] == id {
				delete(v.byFP, rec.Fingerprint)
			}
			n++
		}
	}
	if n > 0 {
		v.dirty = true
	}
	v.mu.Unlock()
	if n > 0 {
		v.maybeSave()
	}
	return n
}

// maybeSave 节流落盘。写失败只记日志：落盘是「让重启不丢」，不该影响请求结果。
func (v *videoStore) maybeSave() {
	if v.path == "" {
		return
	}
	v.mu.Lock()
	if !v.dirty || v.saving {
		v.mu.Unlock()
		return
	}
	if time.Since(v.lastSave) < videoSaveInterval {
		// 节流窗口内：起一个延迟写，保证最后一次改动一定落盘。
		delay := videoSaveInterval - time.Since(v.lastSave)
		v.saving = true
		v.mu.Unlock()
		time.AfterFunc(delay, func() {
			v.mu.Lock()
			v.saving = false
			v.mu.Unlock()
			v.maybeSave()
		})
		return
	}
	v.saving = true
	v.lastSave = time.Now()
	v.dirty = false
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		v.saving = false
		v.mu.Unlock()
	}()
	v.flush()
}

// Flush 立即落盘（进程退出前调用）。
func (v *videoStore) Flush() {
	if v.path == "" {
		return
	}
	v.mu.Lock()
	v.dirty = false
	v.lastSave = time.Now()
	v.mu.Unlock()
	v.flush()
}

func (v *videoStore) flush() {
	v.mu.Lock()
	f := videoStoreFile{Version: videoStoreVersion, Items: make([]*videoRecord, 0, len(v.items))}
	for _, rec := range v.items {
		cp := *rec
		f.Items = append(f.Items, &cp)
	}
	path := v.path
	v.mu.Unlock()

	sort.Slice(f.Items, func(i, j int) bool { return f.Items[i].CreatedAt < f.Items[j].CreatedAt })
	body, err := json.Marshal(&f)
	if err != nil {
		log.Printf("[qwen2api] 序列化视频任务记录失败：%v", err)
		return
	}
	if err := writeFileAtomic0600(path, body); err != nil {
		log.Printf("[qwen2api] 写入视频任务记录失败（%s）：%v", path, err)
	}
}

// writeFileAtomic0600 同目录临时文件 + rename。跨设备 rename 会 EXDEV，
// 所以临时文件必须与目标同目录（data/ 通常是独立挂载卷）。
func writeFileAtomic0600(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".videos-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
