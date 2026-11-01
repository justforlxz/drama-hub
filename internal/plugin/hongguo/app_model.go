package hongguo

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// App 取流接口的数据形状（MKACG-630）：请求体、响应外壳与片源列表。
//
// 只描述上游给什么，不含任何判断逻辑；协议事实（字段名、两种 video_list 形态、
// Code 的两种大小写）在各自字段注释里。判断与选流在 app.go。

// appRequestBody 是取流请求体。biz_param 里的两个开关不能省：
// need_all_video_definition 决定回不回来全部清晰度，video_platform 决定走播放器通道。
type appRequestBody struct {
	BizParam        appBizParam         `json:"biz_param"`
	MixedVideoIDMap map[string][]string `json:"mixed_video_id_map"`
}

type appBizParam struct {
	DetailPageVersion      int  `json:"detail_page_version"`
	DeviceLevel            int  `json:"device_level"`
	DisableDiggStat        bool `json:"disable_digg_stat"`
	NeedAllVideoDefinition bool `json:"need_all_video_definition"`
	NeedMP4Align           bool `json:"need_mp4_align"`
	UseOSPlayer            bool `json:"use_os_player"`
	UseServerDNS           bool `json:"use_server_dns"`
	VideoPlatform          int  `json:"video_platform"`
}

// appEnvelope 是取流响应外壳：Code 两种大小写都出现过。
type appEnvelope struct {
	Code      json.RawMessage            `json:"Code"`
	CodeLower json.RawMessage            `json:"code"`
	Data      map[string]json.RawMessage `json:"data"`
}

// appEntry 是 data[vid] 的载荷：video_model 可能是内联对象，也可能是它的 JSON 文本。
type appEntry struct {
	VideoModel json.RawMessage `json:"video_model"`
}

// appModel 是一集的片源模型。key_seed 存在时，片源地址是密文（AES-128-CBC）。
type appModel struct {
	VideoList appVideoList `json:"video_list"`
	KeySeed   string       `json:"key_seed"`
}

// appVideo 是片源列表里的一行。
type appVideo struct {
	MainURL       string         `json:"main_url"`
	BackupURL     string         `json:"backup_url"`
	BackupURLList []string       `json:"backup_url_list"`
	CodecType     string         `json:"codec_type"`
	Width         int            `json:"width"`
	Height        int            `json:"height"`
	VWidth        int            `json:"vwidth"`
	VHeight       int            `json:"vheight"`
	VideoMeta     appVideoMeta   `json:"video_meta"`
	EncryptInfo   appEncryptInfo `json:"encrypt_info"`
}

// appVideoMeta 是 video_meta 段：编码、尺寸与码率。
type appVideoMeta struct {
	CodecType string `json:"codec_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	VWidth    int    `json:"vwidth"`
	VHeight   int    `json:"vheight"`
	Bitrate   int    `json:"bitrate"`
}

// appEncryptInfo 是加密声明：spade_a 编码内容密钥，encrypt 显式声明是否加密。
type appEncryptInfo struct {
	SpadeA  string          `json:"spade_a"`
	Encrypt json.RawMessage `json:"encrypt"`
}

// appVideoList 兼容上游两种形态：以清晰度为键的对象，或数组。
// 统一成切片，顺序按清晰度键稳定排序——选流结论不能依赖 map 迭代序。
type appVideoList []appVideo

func (l *appVideoList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*l = nil
		return nil
	}
	if trimmed[0] == '[' {
		var arr []appVideo
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*l = arr
		return nil
	}
	var byQuality map[string]appVideo
	if err := json.Unmarshal(trimmed, &byQuality); err != nil {
		return err
	}
	keys := make([]string, 0, len(byQuality))
	for k := range byQuality {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]appVideo, 0, len(keys))
	for _, k := range keys {
		out = append(out, byQuality[k])
	}
	*l = out
	return nil
}

// codec 取本行的编码名（行上优先，video_meta 兜底）。
func (v appVideo) codec() string {
	if v.CodecType != "" {
		return v.CodecType
	}
	return v.VideoMeta.CodecType
}

// candidates 返回本行的候选地址（主 → 备 → 备用列表），空行被跳过。
func (v appVideo) candidates() []string {
	out := make([]string, 0, 2+len(v.BackupURLList))
	for _, raw := range append([]string{v.MainURL, v.BackupURL}, v.BackupURLList...) {
		if strings.TrimSpace(raw) != "" {
			out = append(out, raw)
		}
	}
	return out
}

// size 返回短边像素与码率：尺寸缺失记 0（排序时靠后），不参与「可播」判定。
func (v appVideo) size() (pixels, bitrate int) {
	for _, pair := range [][2]int{
		{v.VWidth, v.VHeight}, {v.Width, v.Height},
		{v.VideoMeta.VWidth, v.VideoMeta.VHeight}, {v.VideoMeta.Width, v.VideoMeta.Height},
	} {
		if pair[0] > 0 && pair[1] > 0 {
			pixels = pair[0]
			if pair[1] < pixels {
				pixels = pair[1]
			}
			break
		}
	}
	return pixels, v.VideoMeta.Bitrate
}

// appCodecRank 给编码排序：0 = h264（Emby 兼容性最好），1 = hevc，-1 = 不认。
func appCodecRank(codec string) int {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "h264", "avc", "avc1":
		return 0
	case "hevc", "h265", "h265_hvc1", "hvc1", "hev1", "bytevc1":
		return 1
	}
	return -1
}
