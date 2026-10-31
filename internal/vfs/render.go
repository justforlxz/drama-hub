package vfs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/justforlxz/drama-hub/internal/store"
)

// xmlHeader 是 NFO 的固定头（standalone 声明由 Emby / Kodi 双方接受）。
const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>` + "\n"

// baseURLKey 是请求上下文中「本服务对外基地址」（`scheme://host`）的键，由 webdav 层
// 按请求 Host 放进来。strm_prefix 为空时 .strm 用它生成绝对地址（MKACG-616 B1）：
// Emby 与第三方播放器只认绝对 URL，相对路径 ` /video/…` 根本放不了。
type baseURLKey struct{}

// WithBaseURL 把一个请求的基地址放进上下文；空串等价于没放（渲染退回相对路径）。
func WithBaseURL(ctx context.Context, base string) context.Context {
	if base == "" {
		return ctx
	}
	return context.WithValue(ctx, baseURLKey{}, base)
}

// baseURLFrom 取上下文里的请求基地址；没有（单测、内部调用）返回空串。
func baseURLFrom(ctx context.Context) string {
	base, _ := ctx.Value(baseURLKey{}).(string)
	return base
}

// renderStrm 渲染 strm 内容：一行 URL + 尾换行。
//
// 形态 `{strm_prefix}/video/{id}.m3u8\n`（docs/PLAYBACK-CONTRACT.md「.strm 的内容」）：
// 单集与合并集没有区别，对外统一是 HLS 播放列表。uid 取 emby_item.uid（单集
// `e<episode.id>`、合并集 `m<group.id>`），派生表的 uid 是唯一真值（硬约束 6）。
// id 由注入的 IDFunc 计算，`/video` 端点本身属 S8。strm_prefix 与 stream_secret
// 每次现读，改值即时生效（硬约束 5）。缺 stream_secret 时透传 store.ErrSettingMissing。
//
// 前缀的优先级：settings 的 strm_prefix → 请求基地址（空 = 按 Host 推导）→ 相对路径
// （没有请求上下文时，只出现在单测/内部调用）。两个来源都去掉尾 `/`：`{prefix}/video/`
// 是字符串拼接，留着会产出 `//video/`（MKACG-617 实测）。
func (t *Tree) renderStrm(ctx context.Context, item store.EmbyItem) ([]byte, error) {
	prefix, err := t.settings.String(ctx, store.KeyStrmPrefix)
	if err != nil {
		return nil, fmt.Errorf("读 strm_prefix: %w", err)
	}
	secret, err := t.settings.String(ctx, store.KeyStreamSecret)
	if err != nil {
		return nil, fmt.Errorf("读 stream_secret: %w", err)
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		prefix = strings.TrimRight(baseURLFrom(ctx), "/")
	}
	return []byte(prefix + "/video/" + t.id(secret, item.UID) + ".m3u8\n"), nil
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

// renderItemNFO 渲染集 NFO：根 episodedetails，字段 title/season/episode/runtime。
//
// 字段全部来自 emby_item 派生表：title 与文件名共用 embyLabel，episode 是对外
// 集号（合并集也是 1 集），runtime 取 runtime_sec/60 向下取整（短于 60 秒为 0，
// Emby 视为未知，由探测媒体文件补）。
func renderItemNFO(item store.EmbyItem) ([]byte, error) {
	return marshalNFO(episodeNFO{
		Title:   embyLabel(item),
		Season:  item.Season,
		Episode: item.EpNo,
		Runtime: item.RuntimeSec / 60,
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
