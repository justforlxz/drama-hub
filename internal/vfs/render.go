package vfs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"

	"github.com/justforlxz/drama-hub/internal/store"
)

// xmlHeader 是 NFO 的固定头（standalone 声明由 Emby / Kodi 双方接受）。
const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>` + "\n"

// renderStrm 渲染 strm 内容：一行 URL + 尾换行。
//
// 形态 `{strm_prefix}/s/{token}/e/{uid}\n`。uid = episode 主键十进制；
// token 由注入的 TokenFunc 计算；strm_prefix 与 stream_secret 每次现读，
// 改值即时生效（硬约束 5）。缺 stream_secret 时透传 store.ErrSettingMissing。
func (t *Tree) renderStrm(ctx context.Context, ep store.Episode) ([]byte, error) {
	prefix, err := t.settings.String(ctx, store.KeyStrmPrefix)
	if err != nil {
		return nil, fmt.Errorf("读 strm_prefix: %w", err)
	}
	secret, err := t.settings.String(ctx, store.KeyStreamSecret)
	if err != nil {
		return nil, fmt.Errorf("读 stream_secret: %w", err)
	}
	uid := strconv.FormatInt(ep.ID, 10)
	return []byte(prefix + "/s/" + t.token(secret, uid) + "/e/" + uid + "\n"), nil
}

// episodeNFO 是集 NFO 的 XML 结构，根元素 episodedetails（Emby EpisodeNfoParser 契约）。
//
// runtime 单位是分钟：Emby 用 TimeSpan.FromMinutes 解析，写秒会被放大 60 倍。
type episodeNFO struct {
	XMLName xml.Name `xml:"episodedetails"`
	Title   string   `xml:"title"`
	Season  int      `xml:"season"`
	Episode int      `xml:"episode"`
	Runtime int      `xml:"runtime"`
}

// showNFO 是剧 NFO 的 XML 结构，根元素 tvshow（Emby SeriesNfoParser 契约）。
type showNFO struct {
	XMLName xml.Name `xml:"tvshow"`
	Title   string   `xml:"title"`
	Year    int      `xml:"year"`
	Plot    string   `xml:"plot"`
}

// renderEpisodeNFO 渲染集 NFO：根 episodedetails，字段 title/season/episode/runtime。
//
// title 与文件名共用 episodeBase 的标签部分（同一个 episodeLabel）；runtime 取
// DurationSec/60 向下取整（短于 60 秒为 0，Emby 视为未知，由探测媒体文件补）。
func renderEpisodeNFO(ep store.Episode) ([]byte, error) {
	return marshalNFO(episodeNFO{
		Title:   episodeLabel(ep),
		Season:  1,
		Episode: ep.EpNo,
		Runtime: ep.DurationSec / 60,
	})
}

// renderShowNFO 渲染剧 NFO：根 tvshow，字段 title/year/plot。
func renderShowNFO(d store.Drama) ([]byte, error) {
	return marshalNFO(showNFO{
		Title: d.Title,
		Year:  d.Year,
		Plot:  d.Plot,
	})
}

// marshalNFO 序列化为「固定头 + 两空格缩进体 + 尾换行」的字节。
//
// 长度就是 len(返回值)，没有第二条长度来源。structure 的字段类型保证
// 序列化不会失败（无 chan / func / 不支持的类型），错误仅作防御性返回。
func marshalNFO(v any) ([]byte, error) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 NFO: %w", err)
	}
	out := make([]byte, 0, len(xmlHeader)+len(body)+1)
	out = append(out, xmlHeader...)
	out = append(out, body...)
	out = append(out, '\n')
	return out, nil
}
