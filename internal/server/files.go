package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- OpenAI Files 前门 ----------
//
// 上游没有"文件库"：文件是**跟着消息走**的 —— 先 `getstsToken` 上传拿到 `files[]` 条目，
// 再把条目放进 `messages[0].files`，最后按 **t2t** 提交（不需要特殊 chat_type）。
// 2026-10-28 实测：95 字节 txt 上传后提问，模型原样答出文件里的关键词。
//
// 所以本端点的角色是"把上游的临时条目暂存起来，给调用方一个可复用的 id"：
//
//	POST /v1/files        → {id:"file-…", …}         （真实上传，条目留在内存）
//	POST /v1/chat/completions 的 content part
//	     {"type":"file","file":{"file_id":"file-…"}}  → 提交时拼回 files[]
//
// 🔴 条目只在内存留 2 小时，且**与上传它的账号绑定**（file_id 是账号维度资源）。
// 跨账号复用会被上游判无权访问 —— 这点与图片参考图完全一致。

// FileStore 上传条目的内存暂存（2h TTL）。
type FileStore struct {
	mu    sync.Mutex
	items map[string]*storedFile
}

type storedFile struct {
	account   string
	entry     upstream.FileRef
	filename  string
	mime      string
	bytes     int
	createdAt time.Time
	purpose   string
}

// NewFileStore 构造。
func NewFileStore() *FileStore { return &FileStore{items: map[string]*storedFile{}} }

func (f *FileStore) put(id string, sf *storedFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = sf
	for k, v := range f.items {
		if time.Since(v.createdAt) > 2*time.Hour {
			delete(f.items, k)
		}
	}
}

func (f *FileStore) get(id string) (*storedFile, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sf, ok := f.items[id]
	if !ok || time.Since(sf.createdAt) > 2*time.Hour {
		return nil, false
	}
	return sf, true
}

// FileObject `POST /v1/files` 的响应（OpenAI 形态）。
type FileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int    `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

// handleFiles `POST /v1/files`（multipart：file=@x + purpose）。
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"需要 multipart/form-data（字段 file）: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", "缺少 file 字段", "file")
		return
	}
	fh := files[0]
	f, err := fh.Open()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "打开上传文件失败: "+err.Error())
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取上传文件失败: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", "文件为空", "file")
		return
	}

	mime := fh.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	purpose := strings.TrimSpace(r.FormValue("purpose"))
	if purpose == "" {
		purpose = "assistants"
	}

	// 上传与后续提问必须同一账号 ⇒ 当场 Pick 一个并记下名字。
	a, ok := s.pool.Pick()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", errNoAccount.Error())
		return
	}
	// 文件是账号维度资源：上传时用哪个出口，后续生成也得用同一条。
	actx := upstream.WithAccount(r.Context(), a.Name)
	// 🔴 上传前确保凭据新鲜：getstsToken 对过期 Bearer 不返回 401，
	// 而是 HTTP 200 + WAF 挑战页，会让"401 才续期"的兜底完全失效。
	cred := s.ensureFresh(actx, a, s.pool.Credential(a.Name))
	entry, upErr := s.up.UploadAttachment(actx, cred, fh.Filename, mime, data)
	s.pool.Release(a)
	if upErr != nil {
		s.noteFailure(a, upErr)
		writeError(w, httpStatusFor(upErr), errTypeFor(upErr), upErr.Error())
		return
	}
	s.pool.MarkOK(a)

	id := "file-" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	s.files.put(id, &storedFile{
		account: a.Name, entry: entry, filename: fh.Filename, mime: mime,
		bytes: len(data), createdAt: time.Now(), purpose: purpose,
	})

	writeJSON(w, http.StatusOK, FileObject{
		ID: id, Object: "file", Bytes: len(data),
		CreatedAt: time.Now().Unix(), Filename: fh.Filename, Purpose: purpose,
	})
}

// handleFileGet `GET /v1/files/{id}` —— 元数据（不含内容，上游不接受回读原文件）。
func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/files/")
	sf, ok := s.files.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			"未知的 file id（暂存 2 小时，进程重启后失效）")
		return
	}
	writeJSON(w, http.StatusOK, FileObject{
		ID: id, Object: "file", Bytes: sf.bytes,
		CreatedAt: sf.createdAt.Unix(), Filename: sf.filename, Purpose: sf.purpose,
	})
}

// collectFiles 从整轮 messages 里收集 file_id，解析成上游 files[] 条目。
//
// 支持两种写法（都在 OpenAI 生态里出现过）：
//
//	{"type":"file","file":{"file_id":"file-…"}}
//	{"type":"input_file","file_id":"file-…"}
//
// 同一条消息里多个 file part 会按出现顺序保留。
func (s *Server) collectFiles(msgs []ChatMessage) ([]upstream.FileRef, error) {
	var out []upstream.FileRef
	for _, m := range msgs {
		for _, p := range m.parts() {
			var id string
			switch p.Type {
			case "file", "input_file":
				id = p.FileID
				if id == "" && p.File != nil {
					id = p.File.FileID
				}
			default:
				continue
			}
			if id == "" {
				continue
			}
			sf, ok := s.files.get(id)
			if !ok {
				return nil, fmt.Errorf("未知或已过期的 file_id: %s"+
					"（/v1/files 上传的条目只在内存留 2 小时）", id)
			}
			out = append(out, sf.entry)
		}
	}
	return out, nil
}

var _ = json.Marshal
