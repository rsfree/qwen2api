package upstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---------- 附件上传链 ----------
//
// 图生图/文件解析的必经之路：外链 URL 上游不认，必须先把字节传到它自己的 OSS。
// 三步（2026-09-24 由 rsfree/qwen 端到端验证，image-adapter 侧亦有 20 张输入图实测）：
//
//  1. `POST /api/v2/files/getstsToken` body `{"filename","filesize"(字符串),"filetype"}`
//     → `{access_key_id, access_key_secret, security_token, bucketname, endpoint,
//        file_path, file_url, file_id, region}`
//  2. `PUT https://{bucket}.{endpoint}/{file_path}`，**OSS V4 头签名 + UNSIGNED-PAYLOAD**
//  3. 组装前端形状的 `files[]` 条目（字段一个都不能省，少了上游当没传）
//
// 🔴 三个签名陷阱（都实测过）：
//   - 派生密钥前缀是 **`aliyun_v4`**，不是 `aliyun_v4_request`（后者是 scope 尾段）；
//   - `region` 要把返回值的 `oss-` 前缀剥掉，否则签名 region 不匹配；
//   - 必须带 `x-oss-security-token`（STS 临时凭证），并计入签名头。

// STSToken getstsToken 的响应。
type STSToken struct {
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	SecurityToken   string `json:"security_token"`
	BucketName      string `json:"bucketname"`
	Endpoint        string `json:"endpoint"`
	FilePath        string `json:"file_path"`
	FileURL         string `json:"file_url"`
	FileID          string `json:"file_id"`
	Region          string `json:"region"`

	size int64
}

// filetype 由 MIME 推断 —— 前端的 getFileType 只有这四类。
//
// 传错会得到 `Bad_Request: Invalid file information!`（形态不对，不是"不支持"）。
func fileTypeOf(mime string) string {
	low := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(low, "image"):
		return "image"
	case strings.HasPrefix(low, "video"):
		return "video"
	case strings.HasPrefix(low, "audio"):
		return "audio"
	}
	return "file"
}

// entryShapes kind → (showType, file_class)。四类逐项对齐前端。
var entryShapes = map[string][2]string{
	"image": {"image", "vision"},
	"file":  {"file", "document"},
	"audio": {"audio", "audio"},
	"video": {"video", "video"},
}

// GetSTSToken 申请一次上传资格（**每张图一次**：STS 凭证被 SessionPolicy 钉在单个 key 上，
// 复用别人的凭证签另一个路径会 403 AccessDenied）。
func (c *Client) GetSTSToken(ctx context.Context, cred, filename, mime string, size int64) (*STSToken, error) {
	body, err := json.Marshal(map[string]string{
		"filename": filename,
		"filesize": fmt.Sprintf("%d", size),
		"filetype": fileTypeOf(mime),
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v2/files/getstsToken", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(cred, c.base+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("getstsToken", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, transportErr("getstsToken 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "getstsToken")
	if err != nil {
		return nil, err
	}
	var sts STSToken
	if err := json.Unmarshal(payload, &sts); err != nil {
		return nil, NewErr(KindUnknown, "getstsToken", resp.StatusCode, "", "响应不是预期 JSON", false)
	}
	missing := []string{}
	for name, v := range map[string]string{
		"access_key_id": sts.AccessKeyID, "access_key_secret": sts.AccessKeySecret,
		"bucketname": sts.BucketName, "endpoint": sts.Endpoint, "file_path": sts.FilePath,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, NewErr(KindUnknown, "getstsToken", resp.StatusCode, "",
			"响应缺字段: "+strings.Join(missing, ","), false)
	}
	sts.size = size
	return &sts, nil
}

// UploadAttachment 完整上传：STS → 签名 PUT → files[] 条目。
//
// kind ∈ image / file / audio / video。返回值可直接塞进 `messages[0].files`。
func (c *Client) UploadAttachment(ctx context.Context, cred, filename, mime string,
	data []byte) (map[string]any, error) {

	if mime == "" {
		mime = "application/octet-stream"
	}
	kind := fileTypeOf(mime)
	sts, err := c.GetSTSToken(ctx, cred, filename, mime, int64(len(data)))
	if err != nil {
		return nil, err
	}
	host, region := ossTarget(sts)
	encodedPath := encodeOSSPath("/" + sts.FilePath)
	putURL := "https://" + host + encodedPath
	canonicalPath := "/" + sts.BucketName + "/" + sts.FilePath

	headers := signOSSV4(sts, canonicalPath, mime, region, time.Now())
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("OSS PUT", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, NewErr(KindUnknown, "OSS PUT", resp.StatusCode, "",
			"上传被拒: "+truncate(string(snippet), 200), false)
	}
	return buildFileEntry(kind, filename, mime, sts), nil
}

// buildFileEntry 组装前端形状的 files[] 条目。
//
// 🔴 字段一个都不能省：上游按存在性判断"这张图传没传完"，
// 少 `status`/`greenNet`/`itemId` 会被当成未完成附件而忽略（静默，无报错）。
func buildFileEntry(kind, filename, mime string, sts *STSToken) map[string]any {
	show, class := "file", "document"
	if v, ok := entryShapes[kind]; ok {
		show, class = v[0], v[1]
	}
	itemID := sts.FileID
	if itemID == "" {
		itemID = strings.ReplaceAll(UUID(), "-", "")
	}
	return map[string]any{
		"type":            show,
		"file":            map[string]any{},
		"id":              sts.FileID,
		"url":             sts.FileURL,
		"name":            filename,
		"collection_name": "",
		"progress":        100,
		"status":          "uploaded",
		"greenNet":        "success",
		"size":            sts.size,
		"error":           "",
		"itemId":          itemID,
		"file_type":       mime,
		"showType":        show,
		"file_class":      class,
	}
}

// ossTarget (bucket 限定 host, 裸 region)。
func ossTarget(sts *STSToken) (host, region string) {
	endpoint := strings.TrimSpace(sts.Endpoint)
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	endpoint = strings.TrimRight(endpoint, "/")
	bucket := strings.TrimSpace(sts.BucketName)
	if strings.HasPrefix(endpoint, bucket+".") {
		host = endpoint
	} else {
		host = bucket + "." + endpoint
	}
	region = strings.TrimSpace(sts.Region)
	region = strings.TrimPrefix(region, "oss-")
	if region == "" {
		region = "cn-hangzhou"
	}
	return host, region
}

// encodeOSSPath 路径编码：保留 `/` 与 unreserved 字符。
func encodeOSSPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// signOSSV4 OSS V4 头签名（UNSIGNED-PAYLOAD）。
func signOSSV4(sts *STSToken, canonicalPath, mime, region string, now time.Time) map[string]string {
	date := now.UTC().Format("20060102")
	ts := now.UTC().Format("20060102T150405Z")
	scope := date + "/" + region + "/oss/aliyun_v4_request"
	const payloadHash = "UNSIGNED-PAYLOAD"

	signable := map[string]string{
		"x-oss-content-sha256": payloadHash,
		"x-oss-date":           ts,
		"content-type":         mime,
	}
	if sts.SecurityToken != "" {
		signable["x-oss-security-token"] = sts.SecurityToken
	}

	// canonical headers：按 key 字典序，每行 `k:v\n`。
	keys := make([]string, 0, len(signable))
	for k := range signable {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var ch strings.Builder
	for _, k := range keys {
		ch.WriteString(k)
		ch.WriteString(":")
		ch.WriteString(strings.TrimSpace(signable[k]))
		ch.WriteString("\n")
	}

	canonicalRequest := strings.Join([]string{
		http.MethodPut,
		encodeOSSPath(canonicalPath),
		"",          // query 为空
		ch.String(), // 已以 \n 结尾
		"",          // signed headers 列表为空
		payloadHash,
	}, "\n")

	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"OSS4-HMAC-SHA256", ts, scope, hex.EncodeToString(sum[:]),
	}, "\n")

	key := hmacSHA256([]byte("aliyun_v4"+sts.AccessKeySecret), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "oss")
	key = hmacSHA256(key, "aliyun_v4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	out := map[string]string{}
	for k, v := range signable {
		out[k] = v
	}
	out["Authorization"] = "OSS4-HMAC-SHA256 Credential=" + sts.AccessKeyID + "/" + scope +
		",Signature=" + signature
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// sortStrings 小插入排序（只有 4 个键，不值得引 sort 包的多余分配）。
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
