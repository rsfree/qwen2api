package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// resolveImageRef 把 `image` 字段的三种形态统一解成字节。
//
//   - `data:image/png;base64,…` → 直接解码（mime 从 data URI 头取）
//   - `http(s)://…`            → 下载（带 Referer/UA：上游 CDN 有防盗链）
//   - 裸 base64                → 解码后按魔数嗅探 mime（SDK 常把 PNG 声明成 octet-stream）
//
// 三种形态都要收，是 image-adapter 的既定契约（`image` 支持 URL / data URI / 裸 base64）。
func resolveImageRef(ctx context.Context, ref string) (data []byte, mime, filename string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, "", "", errors.New("空的图片引用")
	}

	if strings.HasPrefix(ref, "data:") {
		comma := strings.IndexByte(ref, ',')
		if comma < 0 {
			return nil, "", "", errors.New("data URI 缺少逗号")
		}
		head := ref[len("data:"):comma]
		mime = "image/png"
		if i := strings.IndexByte(head, ';'); i >= 0 {
			if head[:i] != "" {
				mime = head[:i]
			}
		}
		if !strings.Contains(head, "base64") {
			return nil, "", "", errors.New("只支持 base64 编码的 data URI")
		}
		raw, decErr := base64.StdEncoding.DecodeString(ref[comma+1:])
		if decErr != nil {
			return nil, "", "", fmt.Errorf("data URI 解码失败: %w", decErr)
		}
		return raw, mime, "ref" + extForMime(mime), nil
	}

	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		raw, dlErr := fetchMedia(ctx, ref)
		if dlErr != nil {
			return nil, "", "", dlErr
		}
		return raw, sniffMime(raw), "ref" + extForMime(sniffMime(raw)), nil
	}

	// 裸 base64
	raw, decErr := base64.StdEncoding.DecodeString(ref)
	if decErr != nil {
		// 也可能是 URL-safe 变体
		raw, decErr = base64.RawURLEncoding.DecodeString(ref)
		if decErr != nil {
			return nil, "", "", errors.New("既不是 data URI / URL，也不是合法 base64")
		}
	}
	m := sniffMime(raw)
	if m == "" {
		return nil, "", "", errors.New("裸 base64 解出来后不是已知图片格式")
	}
	return raw, m, "ref" + extForMime(m), nil
}

// sniffMime 按魔数嗅探（不信任调用方声明的 Content-Type）。
func sniffMime(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	switch {
	case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case b[0] == 0xFF && b[1] == 0xD8:
		return "image/jpeg"
	case b[0] == 'G' && b[1] == 'I' && b[2] == 'F':
		return "image/gif"
	case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func extForMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".bin"
}

// readMultipartImages 解析 /v1/images/edits 的 multipart（OpenAI 的载体差异，语义同 generations）。
//
// 返回 refs（data URI 形态，方便与 JSON 门共用同一条上传链）。
func readMultipartImages(r *http.Request) (prompt string, refs []string, err error) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return "", nil, fmt.Errorf("multipart 解析失败: %w", err)
	}
	prompt = strings.TrimSpace(r.FormValue("prompt"))

	collect := func(field string) error {
		files := r.MultipartForm.File[field]
		for _, fh := range files {
			f, openErr := fh.Open()
			if openErr != nil {
				return fmt.Errorf("读取 %s 失败: %w", field, openErr)
			}
			raw, readErr := io.ReadAll(io.LimitReader(f, 32<<20))
			f.Close()
			if readErr != nil {
				return readErr
			}
			if len(raw) == 0 {
				// 载体歧义：HTML 未选文件就是长度 0 的 part ⇒ 按"没传"处理。
				continue
			}
			mime := sniffMime(raw)
			if mime == "" {
				return fmt.Errorf("%s 不是已知图片格式", fh.Filename)
			}
			refs = append(refs, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(raw))
		}
		return nil
	}
	if err := collect("image"); err != nil {
		return "", nil, err
	}
	if err := collect("image[]"); err != nil {
		return "", nil, err
	}
	// 文本字段形态的 image（URL）也收。
	for _, v := range r.MultipartForm.Value["image"] {
		if strings.HasPrefix(v, "http") {
			refs = append(refs, v)
		}
	}
	return prompt, refs, nil
}

var _ = time.Second
