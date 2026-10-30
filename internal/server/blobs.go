package server

// 图片外链托管：把 data URI / 裸 base64 落成一个「上游能自己拉」的 URL。
//
// 🔴 为什么需要它（2026-10-30 实测，代价很大的一条教训）：
//
//	上游 files[] 条目里的 `url` 就是模型取图的地方。给它一个公网外链
//	（实测 placehold.co 的红图 / 带字图），模型能准确答出颜色和文字 ⇒
//	**根本不需要走 getstsToken 上传链**。
//
//	而 getstsToken 现在被阿里云 WAF 全局挑战（滑块页）。实测 9 种组合全部失败：
//	Bearer / Cookie(token) / Cookie(token+refresh_token) / 带 bx-ua / 不带 bx-ua /
//	预热拿 acw_tc+x-ap / 换 14 个不同出口 / 直连。同一出口的 /api/v2/models/ 却正常
//	⇒ 是端点级人机验证，不是凭据也不是 IP 问题（见 docs/UPSTREAM.md §6.1）。
//
// 所以当前唯一可用的多模态通路是：**我们自己托管图片，把 URL 交给上游**。
//
// 安全边界（这个端点是免鉴权的，必须守住）：
//   - ID 是 32 位随机十六进制，不可枚举（128 bit）；
//   - 只存内存、2 小时 TTL，进程重启即失效；
//   - 只允许 GET，只回图片字节，不回任何元数据；
//   - 单张上限 20MB（resolveImageRef 侧已限）。

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// blobTTL 托管图片的存活时间。与 FileStore / videoStore 同口径（2 小时）：
// 上游拉图发生在提交后的几秒内，2 小时足够覆盖重试与人工排查。
const blobTTL = 2 * time.Hour

// blobMaxBytes 单张上限（与 fetchMedia / 上传链对齐）。
const blobMaxBytes = 20 << 20

type blobItem struct {
	data      []byte
	mime      string
	createdAt time.Time
}

// BlobStore 内存图片托管。
type BlobStore struct {
	mu    sync.Mutex
	items map[string]*blobItem
	// base 公网基址（如 https://qwen2api.1task.cn）。空则用请求推断。
	base string
}

// NewBlobStore 构造。
func NewBlobStore() *BlobStore {
	return &BlobStore{items: map[string]*blobItem{}}
}

// SetBase 设置公网基址（配置项 blob_base_url）。
func (b *BlobStore) SetBase(base string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.base = strings.TrimRight(strings.TrimSpace(base), "/")
}

// Base 当前公网基址。
func (b *BlobStore) Base() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base
}

// Put 存入一张图片，返回 ID（32 位十六进制）。
func (b *BlobStore) Put(data []byte, mime string) (string, error) {
	if len(data) == 0 {
		return "", errEmptyBlob
	}
	if len(data) > blobMaxBytes {
		return "", errBlobTooLarge
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	if mime == "" {
		mime = "application/octet-stream"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[id] = &blobItem{data: data, mime: mime, createdAt: time.Now()}
	// 顺手清理过期项（写入频率低，扫一遍无所谓）。
	for k, v := range b.items {
		if time.Since(v.createdAt) > blobTTL {
			delete(b.items, k)
		}
	}
	return id, nil
}

// Get 取图。
func (b *BlobStore) Get(id string) (*blobItem, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	it, ok := b.items[id]
	if !ok || time.Since(it.createdAt) > blobTTL {
		return nil, false
	}
	return it, true
}

// Len 当前缓存张数（面板/测试用）。
func (b *BlobStore) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

var (
	errEmptyBlob    = errString("图片数据为空")
	errBlobTooLarge = errString("图片超过 20MB 上限")
)

// errString 极简 error（避免为了两条常量引入 fmt）。
type errString string

func (e errString) Error() string { return string(e) }

// handleBlob `GET /v1/blobs/{id}` —— **免鉴权**：上游来拉图时不会带我们的 API Key。
// 安全性由"不可猜 ID + 短 TTL + 只读"保证，见文件头。
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/blobs/")
	if i := strings.IndexByte(id, '.'); i >= 0 {
		id = id[:i] // 允许带 .png 后缀（有些取图端按扩展名判类型）
	}
	it, ok := s.blobs.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", it.mime)
	h.Set("Content-Length", itoa(len(it.data)))
	h.Set("Cache-Control", "public, max-age=3600")
	// 上游取图可能带 Referer；这里不做防盗链（图本来就是要给它拉的）。
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(it.data)
}
