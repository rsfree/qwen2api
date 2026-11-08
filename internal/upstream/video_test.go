package upstream

// 视频 biz_data 的构造测试。
//
// 这里的字段名不是猜的：上游前端 bundle `qwen-chat-fe/0.3.38` 的 genVideo
// 构造器逐字写着 `attachments:[{type:"image",materialId}]` 与
// `genMode: mode==="multi-reference" ? "multi_ref" : "vid_gen"`。
// 写错了不会报错，只会换来一个没有 task_id 的 200 —— 钱照扣。

import (
	"encoding/json"
	"testing"
)

func TestVideoSpecExBase(t *testing.T) {
	spec := VideoSpecEx("16:9", "720P", 5, true, "happyhorse11", VideoExtras{})
	if spec.ChatType != "t2v" || spec.SubChatType != "zaodian_generate_video" {
		t.Fatalf("会话类型不对: %+v", spec)
	}
	var biz map[string]any
	if err := json.Unmarshal([]byte(spec.BizData), &biz); err != nil {
		t.Fatalf("biz_data 不是合法 JSON 字符串: %v", err)
	}
	if biz["bizScene"] != "genVideo" {
		t.Fatalf("bizScene 应为 genVideo: %v", biz)
	}
	req, _ := biz["req"].(map[string]any)
	if req["rootModel"] != "happyhorse11" || req["genMode"] != "vid_gen" {
		t.Fatalf("req 不对: %v", req)
	}
	params, _ := req["params"].(map[string]any)
	if params["resolution"] != "720P" || params["size"] != "16:9" {
		t.Fatalf("params 不对: %v", params)
	}
	if params["duration"] != float64(5) || params["audio"] != true {
		t.Fatalf("时长/音轨不对: %v", params)
	}
	// 没有参考素材时**不许**出现 attachments 键（空数组也会被上游当参数错误）。
	if _, has := params["attachments"]; has {
		t.Fatalf("无素材时不该下发 attachments: %v", params)
	}
}

func TestVideoSpecExAttachments(t *testing.T) {
	spec := VideoSpecEx("origin", "720P", 5, true, "wan27", VideoExtras{
		GenMode:     "vid_gen",
		Attachments: []string{"img-a", "img-b"},
	})
	var biz map[string]any
	_ = json.Unmarshal([]byte(spec.BizData), &biz)
	req, _ := biz["req"].(map[string]any)
	params, _ := req["params"].(map[string]any)

	arr, ok := params["attachments"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("attachments 应有 2 项: %v", params["attachments"])
	}
	first, _ := arr[0].(map[string]any)
	if first["type"] != "image" || first["materialId"] != "img-a" {
		t.Fatalf("附件形态应为 {type:image, materialId}: %v", first)
	}
	// 顺序即语义：首帧在前，尾帧在后。
	second, _ := arr[1].(map[string]any)
	if second["materialId"] != "img-b" {
		t.Fatalf("附件顺序被打乱: %v", arr)
	}
	// adaptive 发上游是 origin（前端 ns() 的映射），不是 "original"。
	if params["size"] != "origin" {
		t.Fatalf("size 应为 origin: %v", params["size"])
	}
}

func TestVideoSpecExMultiRef(t *testing.T) {
	spec := VideoSpecEx("1:1", "1080P", 10, false, "happyhorse", VideoExtras{
		GenMode:     "multi_ref",
		Attachments: []string{"img-x"},
	})
	var biz map[string]any
	_ = json.Unmarshal([]byte(spec.BizData), &biz)
	req, _ := biz["req"].(map[string]any)
	if req["genMode"] != "multi_ref" {
		t.Fatalf("多参考模式应为 multi_ref: %v", req["genMode"])
	}
	if req["rootModel"] != "happyhorse" {
		t.Fatalf("内核应为 happyhorse: %v", req["rootModel"])
	}
}
