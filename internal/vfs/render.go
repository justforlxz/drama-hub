package vfs

import (
	"context"
	"encoding/json"
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
// 单集与合并集没有区别，对外统一是 HLS 播放列表。uid 由派生规则给出（单集
// `e<episode.id>`、合并集 `m<dramaID>_<from>_<to>`），规则的真值在 internal/store
// （deriveEmbyItems，docs/adr/0014）。
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
// plot 是集简介：上游没有单集简介字段，调用方传剧简介兜底；空串时整个元素不出现
// （空 `<plot></plot>` 会被客户端当成「有简介但为空」）。
type episodeNFO struct {
	XMLName xml.Name `xml:"episodedetails"`
	Title   string   `xml:"title"`
	Season  int      `xml:"season"`
	Episode int      `xml:"episode"`
	Runtime int      `xml:"runtime"`
	Plot    string   `xml:"plot,omitempty"`
}

// showNFO 是剧 NFO 的 XML 结构，根元素 tvshow（Emby SeriesNfoParser 契约）。
//
// Genres / Tags 是切片：每个元素一条 `<genre>` / `<tag>`，nil 或空切片时一个元素
// 都不出现（Emby 把空串标签展示成空条目）。Plot 是完整原文，不做截断。
type showNFO struct {
	XMLName xml.Name   `xml:"tvshow"`
	Title   string     `xml:"title"`
	Year    int        `xml:"year"`
	Plot    string     `xml:"plot"`
	Genres  []string   `xml:"genre"`
	Tags    []string   `xml:"tag"`
	Thumb   *showThumb `xml:"thumb,omitempty"`
}

// showThumb 是剧 NFO 的封面元素：`<thumb aspect="poster">poster.jpg</thumb>`。
//
// 值是**相对路径**（poster.jpg 与 tvshow.nfo 同目录），不写外链：Emby 用同目录文件，
// 外链会在源图换址 / 失效时把客户端指向错内容，也绕过了本服务的缓存与淘汰（MKACG-650）。
type showThumb struct {
	Aspect string `xml:"aspect,attr"`
	Value  string `xml:",chardata"`
}

// renderItemNFO 渲染集 NFO：根 episodedetails，字段
// title/season/episode/runtime/plot。
//
// 字段全部来自运行时派生的对外剧集：title 与文件名共用 embyLabel，episode 是对外
// 集号（合并集也是 1 集），runtime 取派生出的块时长/60 向下取整（短于 60 秒为 0，
// Emby 视为未知，由探测媒体文件补）。plot 由调用方给（当前是剧简介），空则不写元素。
func renderItemNFO(item store.EmbyItem, plot string) ([]byte, error) {
	return marshalNFO(episodeNFO{
		Title:   embyLabel(item),
		Season:  item.Season,
		Episode: item.EpNo,
		Runtime: item.RuntimeSec / 60,
		Plot:    plot,
	})
}

// renderShowNFO 渲染剧 NFO：根 tvshow，字段
// title/year/plot/genre/tag（+ 有封面时的 thumb）。
//
// 分类与标签都来自库里已存的原始数据：genre 是主类型，tags 是上游 tags 的原样
// JSON 数组（保序去重）。`<genre>` 写「主类型 + tags 里其余项」——主类型本身也是
// tags[0]，所以按 genre 优先、其余按原序去重追加，避免同一分类出现两次；
// `<tag>` 写 tags 全量原文。tags 为空或不是合法 JSON 时两者都不写（脏行不能拖垮
// 整个 NFO——分类/标签是可选元数据，标题/简介才是主体）。
//
// cover 由调用方按「这部剧有可用封面」算好（listDrama 同一判据）：没有封面时不写
// `<thumb>`，也不写空元素——空 thumb 会让 Emby 去找一张不存在的图（MKACG-650）。
func renderShowNFO(d store.Drama, cover bool) ([]byte, error) {
	tags := decodeTags(d.Tags)
	nfo := showNFO{
		Title:  d.Title,
		Year:   d.Year,
		Plot:   d.Plot,
		Genres: genreList(d.Genre, tags),
		Tags:   tags,
	}
	if cover {
		nfo.Thumb = &showThumb{Aspect: "poster", Value: posterName}
	}
	return marshalNFO(nfo)
}

// decodeTags 把 drama.tags 的 JSON 数组解成切片；空串/非法 JSON 视为「上游未声明」。
//
// 解出的空串项一并丢掉：上游脏数据 `[""]` 若照写会产出空 `<tag></tag>`，与 genre
// 侧跳过空串的口径不一致（空串标签在客户端会被当成空条目）。
func decodeTags(raw string) []string {
	if raw == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil
	}
	out := tags[:0]
	for _, t := range tags {
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// genreList 组装 `<genre>`：主类型在前，其余 tags 按原序去重追加。
//
// 主类型为空（上游未声明分类）时只留 tags 的去重结果，不产出空 `<genre>`。
func genreList(primary string, tags []string) []string {
	seen := make(map[string]bool, len(tags)+1)
	out := make([]string, 0, len(tags)+1)
	add := func(g string) {
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		out = append(out, g)
	}
	add(primary)
	for _, t := range tags {
		add(t)
	}
	return out
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
