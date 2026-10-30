package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	account  string
	entry    upstream.FileRef
	filename string
	mime     string
	bytes    int
	createdAt time.Time
	purpose  string
	// inline 纯文本附件：不走上游 files[]，直接把内容拼进 prompt。
	//
	// 🔴 为什么要这条支路（2026-10-30）：document 类附件必须是真的 OSS 条目
	// （服务端按 file_id 去 OSS 取，给 url 它不读 —— 实测模型回"无法读取"），
	// 而 OSS 上传口 getstsToken 已被阿里云 WAF 滑块挡住（UPSTREAM §6.1）。
	// 纯文本本来就不需要它：读进 prompt 效果一样，还省一次上传。
	inline bool
	text   string
}

// inlineTextLimit 单个文本附件内联上限（超出截断）。256KB ≈ 64k token，
// 再大多半会顶掉上下文，不如让调用方先摘要。
const inlineTextLimit = 256 << 10

// isTextFile 判断能否按纯文本内联。
//
// 只认"确定能当文本读"的：text/* 前缀、常见结构化文本 MIME、常见源码/配置后缀，
// 且内容必须是合法 UTF-8 —— 二进制文件误判成文本会变成一堆乱码 prompt。
func isTextFile(filename, mime string, data []byte) bool {
	low := strings.ToLower(strings.TrimSpace(mime))
	if strings.HasPrefix(low, "text/") {
		return utf8.Valid(data)
	}
	switch low {
	case "application/json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/x-sh", "application/x-yaml",
		"application/yaml", "application/toml", "application/sql":
		return utf8.Valid(data)
	}
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".log", ".json", ".yaml", ".yml",
		".toml", ".ini", ".conf", ".cfg", ".env", ".xml", ".html", ".htm", ".css",
		".js", ".ts", ".jsx", ".tsx", ".py", ".go", ".rs", ".java", ".kt", ".c", ".h",
		".cpp", ".hpp", ".sh", ".bash", ".zsh", ".sql", ".proto", ".diff", ".patch":
		return utf8.Valid(data)
	}
	return false
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
	// Mode 处理方式：text = 内联进 prompt；oss = 上游 files[] 条目。
	// 加这个字段是为了让调用方能看懂"为什么这个文件不用 file_id 也能问"。
	Mode string `json:"mode,omitempty"`
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

	// 纯文本：直接读进内存，提问时拼进 prompt（不碰 getstsToken —— 它已被 WAF 挡）。
	if isTextFile(fh.Filename, mime, data) {
		text, truncated := string(data), false
		if len(text) > inlineTextLimit {
			text, truncated = text[:inlineTextLimit], true
		}
		id := "file-" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
		s.files.put(id, &storedFile{
			filename: fh.Filename, mime: mime, bytes: len(data),
			createdAt: time.Now(), purpose: purpose, inline: true, text: text,
		})
		writeJSON(w, http.StatusOK, FileObject{
			ID: id, Object: "file", Bytes: len(data), CreatedAt: time.Now().Unix(),
			Filename: fh.Filename, Purpose: purpose, Mode: "text",
		})
		if truncated {
			log.Printf("[qwen2api] 文本附件 %s 超过 %d 字节，已截断内联", fh.Filename, inlineTextLimit)
		}
		return
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
		CreatedAt: time.Now().Unix(), Filename: fh.Filename, Purpose: purpose, Mode: "oss",
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
func (s *Server) collectFiles(msgs []ChatMessage) ([]upstream.FileRef, []attachedText, error) {
	var out []upstream.FileRef
	var texts []attachedText
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
				return nil, nil, fmt.Errorf("未知或已过期的 file_id: %s"+
					"（/v1/files 上传的条目只在内存留 2 小时）", id)
			}
			if sf.inline {
				texts = append(texts, attachedText{filename: sf.filename, text: sf.text})
				continue
			}
			out = append(out, sf.entry)
		}
	}
	return out, texts, nil
}

// attachedText 内联进 prompt 的文本附件。
type attachedText struct {
	filename string
	text     string
}

// appendTextAttachments 把文本附件拼到 prompt 末尾。
//
// 形态刻意简单：分隔线 + 文件名 + 原文。不写"请阅读以下文件"这类引导语 ——
// 上游拿到的就是用户原话 + 附件原文，语义最接近网页端"发文件再提问"。
func appendTextAttachments(prompt string, texts []attachedText) string {
	if len(texts) == 0 {
		return prompt
	}
	var sb strings.Builder
	sb.WriteString(prompt)
	for _, t := range texts {
		sb.WriteString("\n\n---\n附件《")
		sb.WriteString(t.filename)
		sb.WriteString("》内容：\n")
		sb.WriteString(t.text)
	}
	return sb.String()
}

var _ = json.Marshal
