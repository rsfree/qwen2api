package server

// 视频后处理：上游出片 → 去水印 → 上传自建存储 → 才对外交付。
//
// 🔴 核心纪律：**没处理完不发 URL**。
// 上游 `succeeded` 只代表"素材好了"，对外的 `succeeded` 必须等到上传完成。
// 否则客户端会拿到带水印那一版，而它 24h 后就 404 —— 没有补救窗口。

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// videoPolishMaxTries 单条最多重试次数。
//
// 上游产物 24h 有效，所以重试窗口是够的；但每次失败都要重新下载整个视频，
// 次数设太大只是在烧带宽。3 次覆盖的是"网络抖一下""OSS 502"这类瞬时故障。
const videoPolishMaxTries = 3

// videoPolisher 后处理队列。
type videoPolisher struct {
	s  *Server
	ch chan string

	mu   sync.Mutex
	busy map[string]bool // 已在队列/处理中，防重复入队
}

func newVideoPolisher(s *Server, workers int) *videoPolisher {
	if workers <= 0 {
		workers = 1
	}
	p := &videoPolisher{
		s:    s,
		ch:   make(chan string, 256),
		busy: map[string]bool{},
	}
	for i := 0; i < workers; i++ {
		go p.loop()
	}
	return p
}

// enqueue 非阻塞入队。队列满就丢 —— 巡检每秒会重扫未终态任务，
// 丢掉的会在下一轮被重新发现，不需要在这里阻塞等。
func (p *videoPolisher) enqueue(id string) {
	p.mu.Lock()
	if p.busy[id] {
		p.mu.Unlock()
		return
	}
	p.busy[id] = true
	p.mu.Unlock()

	select {
	case p.ch <- id:
	default:
		p.mu.Lock()
		delete(p.busy, id)
		p.mu.Unlock()
		log.Printf("[qwen2api] 后处理队列已满，任务 %s 推迟到下一轮巡检", id)
	}
}

func (p *videoPolisher) loop() {
	for id := range p.ch {
		p.process(id)
		p.mu.Lock()
		delete(p.busy, id)
		p.mu.Unlock()
	}
}

// recover 把重启前卡在 queued/running 的任务重新排上。
//
// 不做这一步的话，容器重启那一刻正在处理的视频会永远停在 polishing ——
// 它既不是终态（客户端一直等到超时）、又没有任何东西会再推进它。
func (p *videoPolisher) recover() {
	for _, rec := range p.s.videos.Pending() {
		if rec.PolishState == "queued" || rec.PolishState == "running" {
			if rec.UpstreamURL != "" {
				rec.PolishState = "queued"
				p.s.videos.Put(rec)
				p.enqueue(rec.ID)
			}
		}
	}
}

func (p *videoPolisher) process(id string) {
	rec, ok := p.s.videos.Get(id)
	if !ok {
		return
	}
	// 已经交付过的不重复处理（幂等：巡检可能重复触发）。
	if rec.Status == VideoStatusSucceeded && rec.VideoURL != "" {
		return
	}
	if rec.UpstreamURL == "" {
		// 上游还没给地址：退回非终态，等巡检再查。
		rec.Status = VideoStatusRunning
		rec.PolishState = ""
		p.s.videos.Put(rec)
		return
	}

	st := p.s.conf().Storage
	timeout := st.TimeoutS
	if timeout <= 0 {
		timeout = 300
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	rec.PolishState = "running"
	p.s.videos.Put(rec)

	start := time.Now()
	url, err := p.run(ctx, rec, st)
	if err != nil {
		rec.PolishTries++
		rec.PolishErr = err.Error()
		rec.PolishState = "queued"
		if rec.PolishTries >= videoPolishMaxTries {
			rec.PolishState = "failed"
			// 🔴 不降级返回上游直链：那条链接带水印，且 24h 后失效。
			// 与其交一个有瑕疵且会过期的地址，不如明确失败让调用方知道。
			rec.Status = VideoStatusFailed
			rec.ErrCode = "polish_failed"
			rec.ErrMsg = "去水印/托管失败（已重试 " + strconv.Itoa(rec.PolishTries) + " 次）：" + err.Error()
			rec.UpdatedAt = time.Now().Unix()
			p.s.videos.Put(rec)
			log.Printf("[qwen2api] 视频 %s 后处理失败（%d 次）：%v", rec.ID, rec.PolishTries, err)
			return
		}
		p.s.videos.Put(rec)
		log.Printf("[qwen2api] 视频 %s 后处理第 %d 次失败，稍后重试：%v",
			rec.ID, rec.PolishTries, err)
		tries := rec.PolishTries
		go func() {
			time.Sleep(time.Duration(tries) * 5 * time.Second)
			p.enqueue(id)
		}()
		return
	}

	rec.Status = VideoStatusSucceeded
	rec.VideoURL = url
	rec.PolishState = "done"
	rec.PolishErr = ""
	rec.UpdatedAt = time.Now().Unix()
	p.s.videos.Put(rec)
	log.Printf("[qwen2api] 视频 %s 后处理完成（%s）：%s", rec.ID,
		time.Since(start).Round(time.Millisecond), url)
}

// run 走完整条流水线：下载 → 去水印 → 上传。
func (p *videoPolisher) run(ctx context.Context, rec *videoRecord, st config.Storage) (string, error) {
	ffmpeg := st.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}

	raw, err := downloadVideo(ctx, rec.UpstreamURL)
	if err != nil {
		return "", fmt.Errorf("下载上游产物失败: %w", err)
	}

	dir, err := os.MkdirTemp("", "qwen2api-polish-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "src.mp4")
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "out.mp4")

	// watermark=true 表示客户端**要**带水印：上游本来就有，原样交付即可。
	// 只有 false（含默认）才需要抹掉 —— 这才是方舟 watermark 字段的语义。
	needStrip := st.StripWatermark && !rec.Watermark
	if needStrip {
		W, H, fps, err := probeVideoDim(ctx, ffmpeg, src)
		if err != nil {
			return "", fmt.Errorf("探测视频规格失败: %w", err)
		}
		bx, by, bw, bh, measured := watermarkBox(W, H)
		if !measured {
			log.Printf("[qwen2api] 视频 %s 短边 %d != %d，水印框按比例外推（无实测依据）",
				rec.ID, minInt(W, H), baseShort)
		}
		bx, by, bw, bh, err = normalizeBox(bx, by, bw, bh, W, H)
		if err != nil {
			return "", err
		}
		quality := st.Quality
		if quality == "" {
			quality = "near"
		}
		if err := transcodeStrip(ctx, ffmpeg, src, dst, W, H, fps,
			[4]int{bx, by, bw, bh}, quality); err != nil {
			return "", fmt.Errorf("转码失败: %w", err)
		}
	} else {
		if err := os.Rename(src, dst); err != nil {
			return "", err
		}
	}

	body, err := os.ReadFile(dst)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", fmt.Errorf("产物为空")
	}

	// Prefix 由 s3PutObject 统一拼接 —— 这里再拼一次会得到
	// "prefix/prefix/id.mp4"（实测踩过）。
	return s3PutObject(ctx, st, rec.ID+".mp4", body, "video/mp4")
}

// downloadVideo 取上游成片。
//
// 不能复用 fetchMedia：那个上限 40MB、超时 90s，是给图片设计的；
// 1080p/15s 的成片可能上百 MB，且上游 CDN 在跨境链路上偶尔要几十秒。
func downloadVideo(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 上游 CDN 的 WAF 只放行浏览器类 UA，curl 默认 UA 会拿到 404。
	req.Header.Set("Referer", "https://chat.qwen.ai/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if len(raw) < 1024 {
		return nil, fmt.Errorf("产物过小（%d 字节），疑似死链", len(raw))
	}
	return raw, nil
}

// probeVideoDim 读宽高与帧率。
func probeVideoDim(ctx context.Context, ffmpegPath, src string) (int, int, float64, error) {
	ffprobe := "ffprobe"
	if strings.HasSuffix(ffmpegPath, "ffmpeg") {
		ffprobe = strings.TrimSuffix(ffmpegPath, "ffmpeg") + "ffprobe"
	}
	out, err := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height,r_frame_rate",
		"-of", "csv=p=0", src).Output()
	if err != nil {
		return 0, 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) < 3 {
		return 0, 0, 0, fmt.Errorf("ffprobe 输出无法解析: %q", string(out))
	}
	W, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	H, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, 0, fmt.Errorf("ffprobe 宽高非法: %q", string(out))
	}
	fps := 24.0
	if n, d, ok := strings.Cut(strings.TrimSpace(parts[2]), "/"); ok {
		fn, e1 := strconv.ParseFloat(n, 64)
		fd, e2 := strconv.ParseFloat(d, 64)
		if e1 == nil && e2 == nil && fd > 0 && fn > 0 {
			fps = fn / fd
		}
	}
	return W, H, fps, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
