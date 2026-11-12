package server

// S3 兼容对象存储的最小上传实现（AWS SigV4，纯标准库）。
//
// 为什么手写而不引 aws-sdk-go-v2：这个仓库的 go.mod 是**零第三方依赖**，
// 为了一次 PUT 拉进整棵 SDK（及其传递依赖）不划算。SigV4 的算法是固定的，
// 手写约 100 行，且行为可测。
//
// 只实现 PUT —— 网关只需要"把处理好的成片放上去"。列桶/删桶交给运维工具。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// s3HTTPClient 专用于对象存储的客户端。
//
// 🔴 不复用 http.DefaultClient：默认值会继承 HTTP_PROXY（本机/容器里一旦设了，
// 跨境上传会被绕到代理再出去，实测从 1.6s 变成 20s 超时）。
var s3HTTPClient = &http.Client{
	Timeout: 300 * time.Second,
	Transport: &http.Transport{
		Proxy:                 nil,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
}

// awsURIEncode SigV4 要求的路径编码：除 A-Za-z0-9-_.~ 外全部 %XX。
// encodeSlash=false 时保留 '/' 作为路径分隔符。
//
// 🔴 不能用 url.QueryEscape：它把空格编成 '+'，SigV4 要 '%20'，
// 签名和实际请求就会对不上（表现为 403 SignatureDoesNotMatch，极难排查）。
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// s3PutObject 上传一个对象并返回它的公开访问 URL。
//
// endpoint 形如 https://oss.s3ai.cn（不含 bucket，path-style 访问）。
func s3PutObject(ctx context.Context, sc config.Storage, key string, body []byte, contentType string) (string, error) {
	if sc.Endpoint == "" || sc.Bucket == "" {
		return "", fmt.Errorf("storage 未配置 endpoint/bucket")
	}
	u, err := url.Parse(sc.Endpoint)
	if err != nil {
		return "", fmt.Errorf("storage.endpoint 非法: %w", err)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := u.Host
	if host == "" {
		return "", fmt.Errorf("storage.endpoint 缺少 host")
	}

	region := sc.Region
	if region == "" {
		region = "us-east-1"
	}
	// 对象路径：/bucket/prefix/key（每段独立编码，'/' 保留）
	objectPath := "/" + sc.Bucket
	if sc.Prefix != "" {
		objectPath += "/" + strings.Trim(sc.Prefix, "/")
	}
	objectPath += "/" + strings.TrimLeft(key, "/")
	canonicalURI := awsURIEncode(objectPath, false)

	payloadHash := sha256Hex(body)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	// 参与签名的头必须与真正发出去的头逐字一致（排序 + 小写）。
	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if contentType != "" {
		headers["content-type"] = contentType
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		http.MethodPut,
		canonicalURI,
		"", // 无 query
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+sc.SecretKey), dateStamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(k, stringToSign))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sc.AccessKey, scope, signedHeaders, signature)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		scheme+"://"+host+canonicalURI, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// 跨境链路实测会偶发 TLS 握手超时（本地 110KB/s、生产 650KB/s），
	// 所以：握手给足 30s、不继承 HTTP_PROXY（对象存储是公网站点，
	// 走代理只会把链路绕得更远），并对瞬时错误重试一次。
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := s3HTTPClient.Do(req.Clone(ctx))
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body2, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			goto done
		}
		lastErr = fmt.Errorf("上传返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body2)))
		// 4xx 是配置/签名问题，重试没有意义。
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return "", lastErr
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("上传失败: %w", lastErr)

done:

	base := sc.PublicBase
	if base == "" {
		base = strings.TrimRight(scheme+"://"+host, "/") + "/" + sc.Bucket
	}
	// 🔴 返回的路径必须和 objectPath 一致（含 prefix）。
	// 用裸 key 拼会少一段 → 上传 200 但回读 404，且极难联想到是拼接问题。
	relKey := strings.TrimLeft(key, "/")
	if sc.Prefix != "" {
		relKey = strings.Trim(sc.Prefix, "/") + "/" + relKey
	}
	return strings.TrimRight(base, "/") + "/" + relKey, nil
}
