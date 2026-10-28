package upstream

import (
	"strings"
	"testing"
)

// 2026-10-28 实测帧：t2i 走 phase=image_gen，content 直接是图片直链。
const frameImageGen = `{"choices":[{"delta":{"role":"assistant","content":"https://cdn.qwenlm.ai/output/44444444/t2i/aaaa0001/17931853541671.png?key=TESTKEY","phase":"image_gen","extra":{"output_image_hw":[["928","1664"]],"model":"qwen-image-3.0"},"status":"typing"}}],"response_id":"aaaa0001","usage":{},"timestamp":1793185356}`

const frameImageGenDone = `{"choices":[{"delta":{"content":"","role":"assistant","status":"finished","phase":"image_gen"}}],"response_id":"aaaa0001"}`

// t2t 的工具调用路径：arguments 是 prompt，产物在 extra.image_list。
const frameToolArgs = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"image_gen_tool","status":"typing","function_call":{"name":"image_gen","arguments":"{\"prompt\": \"A cute ginger cat, watercolor\"}"},"function_id":"call_1","extra":{}}}],"response_id":"aaaa0002","usage":{}}`

const frameToolDone = `{"choices":[{"delta":{"role":"function","content":"","phase":"image_gen_tool","status":"finished","name":"image_gen","extra":{"function_id":"call_1","tool_result":[{"image":"https://cdn.qwenlm.ai/output/u/t2i/c/1req_a.png?key=JWT"}],"image_list":[{"image":"https://cdn.qwenlm.ai/output/u/image_gen/r/1793184061.png?key=JWT2"}],"display_position":"answer"},"function_id":"call_1"}}],"response_id":"aaaa0002","usage":{},"timestamp":1793184061}`

func TestParserImageGenT2I(t *testing.T) {
	p := NewParser()
	ev, ok, err := p.Parse([]byte(frameImageGen))
	if err != nil || !ok {
		t.Fatalf("image_gen 帧解析失败: ok=%v err=%v", ok, err)
	}
	if len(ev.Images) != 1 {
		t.Fatalf("应提取 1 张图，得到 %d", len(ev.Images))
	}
	if !strings.HasPrefix(ev.Images[0], "https://cdn.qwenlm.ai/") {
		t.Fatalf("图片直链不对: %q", ev.Images[0])
	}
	// 关键：URL 不能被当成正文吐出去。
	if ev.Text != "" {
		t.Fatalf("图片帧不该产出正文: %q", ev.Text)
	}
}

func TestParserImageGenDoneIsNotStreamDone(t *testing.T) {
	// image_gen 的 finished 只表示"这张图完事了"，整条流后面可能还有 answer。
	p := NewParser()
	ev, _, err := p.Parse([]byte(frameImageGenDone))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Done {
		t.Fatal("image_gen finished 不能当成整条流结束")
	}
	if len(ev.Images) != 0 {
		t.Fatal("结束帧不该带图")
	}
}

func TestParserImageGenToolT2T(t *testing.T) {
	p := NewParser()
	ev, _, err := p.Parse([]byte(frameToolArgs))
	if err != nil {
		t.Fatal(err)
	}
	if ev.ImagePrompt != "A cute ginger cat, watercolor" {
		t.Fatalf("工具参数里的 prompt 没提出来: %q", ev.ImagePrompt)
	}
	if ev.ToolName != "image_gen" {
		t.Fatalf("工具名不对: %q", ev.ToolName)
	}
	if len(ev.Images) != 0 {
		t.Fatal("中间帧不该有产物")
	}

	ev, _, err = p.Parse([]byte(frameToolDone))
	if err != nil {
		t.Fatal(err)
	}
	// image_list + tool_result 去重合并 ⇒ 2 个不同 URL。
	if len(ev.Images) != 2 {
		t.Fatalf("应合并出 2 个产物 URL，得到 %d: %v", len(ev.Images), ev.Images)
	}
	if ev.Done {
		t.Fatal("工具结束不等于整条流结束")
	}
}

func TestParserImageToolArgsPartialJSON(t *testing.T) {
	// arguments 是流式累加的，中途常常是非法 JSON —— 必须静默跳过，不能报错。
	p := NewParser()
	ev, ok, err := p.Parse([]byte(`{"choices":[{"delta":{"phase":"image_gen_tool","status":"typing","function_call":{"name":"image_gen","arguments":"{\"prompt\": \"A"}}}]}`))
	if err != nil || !ok {
		t.Fatalf("半截参数不该报错: ok=%v err=%v", ok, err)
	}
	if ev.ImagePrompt != "" {
		t.Fatalf("半截 JSON 不该提取出 prompt: %q", ev.ImagePrompt)
	}
}

func TestBuildSubmitImageSpec(t *testing.T) {
	body := BuildSubmitSpec("chat-1", "", "qwen3.7-plus", "a cat", "fast", ImageSpec("qwen-image-3.0", "16:9"), mustTime())
	if body.Stream != true {
		t.Fatal("t2i 应走流式")
	}
	if body.Size != "16:9" {
		t.Fatalf("顶层 size 丢失: %q", body.Size)
	}
	msg := body.Messages[0]
	if msg.ChatType != "t2i" || msg.SubChatType != "t2i" {
		t.Fatalf("chat_type 标记不对: %q %q", msg.ChatType, msg.SubChatType)
	}
	meta, ok := msg.Extra["meta"].(map[string]any)
	if !ok {
		t.Fatal("extra.meta 不是对象")
	}
	if meta["model"] != "qwen-image-3.0" {
		t.Fatalf("图片模型未写进 extra.meta: %v", meta["model"])
	}
	if meta["size"] != "16:9" {
		t.Fatalf("extra.meta.size 丢失: %v", meta["size"])
	}
	if meta["subChatType"] != "t2i" {
		t.Fatalf("subChatType 不对: %v", meta["subChatType"])
	}
}

func TestBuildSubmitVideoSpec(t *testing.T) {
	spec := VideoSpec("16:9", "720P", 5, true, "happyhorse11")
	body := BuildSubmitSpec("chat-1", "", "qwen3.7-plus", "a cat", "fast", spec, mustTime())

	// 视频必须非流式：上游要同步回 task_id。
	if body.Stream {
		t.Fatal("t2v 必须是 stream:false")
	}
	msg := body.Messages[0]
	// 🔴 两个字段不同值 —— 这是 t2v 最容易写错的地方。
	if msg.ChatType != "t2v" {
		t.Fatalf("chat_type 应为 t2v: %q", msg.ChatType)
	}
	if msg.SubChatType != "zaodian_generate_video" {
		t.Fatalf("sub_chat_type 应为 zaodian_generate_video: %q", msg.SubChatType)
	}
	meta := msg.Extra["meta"].(map[string]any)
	biz, _ := meta["biz_data"].(string)
	if !strings.Contains(biz, `"bizScene":"genVideo"`) ||
		!strings.Contains(biz, `"rootModel":"happyhorse11"`) ||
		!strings.Contains(biz, `"duration":5`) {
		t.Fatalf("biz_data 构造不对: %s", biz)
	}
	// biz_data 必须是**字符串**（对象会被上游 Bad_Request）。
	if _, isObj := meta["biz_data"].(map[string]any); isObj {
		t.Fatal("biz_data 不能是对象")
	}
	if meta["videoGenerationMode"] != "standard" {
		t.Fatalf("videoGenerationMode 丢失: %v", meta["videoGenerationMode"])
	}
}

func TestBuildNewChatTyped(t *testing.T) {
	for _, typ := range []string{"t2t", "t2i", "t2v"} {
		b := BuildNewChatTyped("qwen3.7-plus", typ, mustTime())
		if b.ChatType != typ {
			t.Fatalf("建会话 chat_type 应为 %s，得到 %s", typ, b.ChatType)
		}
	}
}

// 2026-10-28 实测帧：额度耗尽伪装成一条正常 answer（HTTP 200 + 正常收流）。
const frameQuota = `{"choices":[{"delta":{"role":"assistant","content":"今日生图额度已用完，升级会员可获得更多生图额度。","phase":"answer","status":"finished","extra":{"update_member":true}}}],"response_id":"853eaecd","usage":{},"timestamp":1793187976}`

func TestParserQuotaAsAnswer(t *testing.T) {
	p := NewParser()
	ev, ok, err := p.Parse([]byte(frameQuota))
	if err != nil || !ok {
		t.Fatalf("额度帧解析失败: ok=%v err=%v", ok, err)
	}
	if !ev.QuotaExhausted {
		t.Fatal("没识别出额度耗尽（会导致误报成「模型没画」）")
	}
	if !strings.Contains(ev.QuotaMessage, "额度已用完") {
		t.Fatalf("额度文案未保留: %q", ev.QuotaMessage)
	}
	// 它不能同时被当成正文吐出去（否则调用方会看到一句莫名其妙的提示）。
	if ev.Text != "" {
		t.Fatalf("额度提示不该混进正文: %q", ev.Text)
	}
}

func TestParserQuotaByWordingFallback(t *testing.T) {
	// 上游不一定每次都带 update_member ⇒ 文案兜底。
	p := NewParser()
	ev, _, err := p.Parse([]byte(`{"choices":[{"delta":{"content":"今日生图额度已用完，升级会员可获得更多生图额度。","phase":"answer","status":"typing"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ev.QuotaExhausted {
		t.Fatal("文案兜底未生效")
	}
	// 正常文本不能被误判
	ev, _, _ = p.Parse([]byte(`{"choices":[{"delta":{"content":"这是一个圆形，红色。","phase":"answer","status":"typing"}}]}`))
	if ev.QuotaExhausted {
		t.Fatal("正常文本被误判成额度提示")
	}
}
