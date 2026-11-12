package server

// 图片后处理：上游出图 → 去水印 → 上传自建存储 → 才对外交付。
//
// 与视频管线的差别：视频是异步任务（videoPolisher 队列），图片是**同步**返回，
// 所以这里直接在响应前处理，不排队。
//
// 🔴 失败**不降级**（2026-11-12 用户明令："图片也必须无水印"）。
// 早先的实现是失败就返回上游原链 —— 那等于静默交付一张带 18.5% 宽水印的图，
// 调用方无从察觉。现在处理失败宁可让这张图不产出（错误向上抛），
// 也不交一个"看起来成功、实际带水印"的结果。
//
// 存储复用 config.Storage（与视频同一套 endpoint / bucket / 凭据），
// 只是 prefix 换成空的、key 自带 qwen2api/images/ 前缀，避免和视频混在一起。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"log"
	"sync"
	"time"

	// 注册解码器：上游产物是 PNG，但历史/异常情况下也可能是 JPEG
	_ "image/jpeg"
	_ "image/png"
)

// storageWarnOnce 让"storage 未配置"只打印一次，不随每张图刷屏。
var storageWarnOnce sync.Once

// imageObjectKey 生成对象键：qwen2api/images/<YYYYMMDD>/<32位hex>.png
//
// 32 位随机 hex（128 bit）不可枚举 —— 该 bucket 是公开读的，
// 键必须够随机，否则等于让别人遍历我们的产物。
func imageObjectKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用是极端情况，退回时间戳（仍有纳秒级唯一性）
		return fmt.Sprintf("qwen2api/images/%s/%d.png", time.Now().Format("20060102"), time.Now().UnixNano())
	}
	return "qwen2api/images/" + time.Now().Format("20060102") + "/" + hex.EncodeToString(b[:]) + ".png"
}

// polishImage 去水印并转存。
//
// 返回 (新URL, 处理后的PNG字节)：
//   - 成功：两者都有，b64_json 模式可直接用字节，省一次下载
//   - 失败：返回 (原URL, nil)，不阻断出图
func (s *Server) polishImage(ctx context.Context, srcURL string) (string, []byte, error) {
	st := s.conf().Storage
	if !st.StripWatermark {
		return srcURL, nil, nil
	}
	if !st.IsEnabled() {
		// 凭据不全：只在第一次打印，避免每张图刷屏
		storageWarnOnce.Do(func() {
			log.Printf("[qwen2api] 图片去水印未启用（storage 缺: %v），图片将透传上游直链",
				st.MissingKeys())
		})
		return srcURL, nil, nil
	}

	// 下载重试：跨境链路偶发超时，一次抖动就把价 1 张额度的图丢掉太亏。
	var raw []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		raw, err = fetchMedia(ctx, srcURL)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
	}
	if err != nil {
		return "", nil, fmt.Errorf("下载失败: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("解码失败: %w", err)
	}

	out, err := stripWatermark(img, nil)
	if err != nil {
		return "", nil, fmt.Errorf("去水印失败: %w", err)
	}

	// prefix 置空：key 已带完整路径，再拼 prefix 会多一段
	st.Prefix = ""
	key := imageObjectKey()
	// 上传单独给一个较宽的上限：3MB 图在跨境链路上偶发 >30s
	upCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	newURL, err := s3PutObject(upCtx, st, key, out, "image/png")
	if err != nil {
		return "", nil, fmt.Errorf("上传失败: %w", err)
	}
	return newURL, out, nil
}
