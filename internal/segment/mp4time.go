package segment

import (
	"encoding/binary"
	"fmt"
	"time"
)

// 为什么出流时要平移 tfdt（ADR-0011 D6'）
//
// 「每集一片」把一集独立 remux 成一个自包含 fMP4，于是每片的时间戳都从自己开头算。
// 合并集把 N 片串成一条 playlist 时，播放端看到的是 N 条各自从 0 起的 PTS。
//
// 实测（容器内 ffmpeg 6.1.2，MKACG-652 功能检测）：
//   - hls demuxer 遇到 `#EXT-X-DISCONTINUITY` 只打印 Skip，**不重定时间基准**；
//   - 于是包 PTS 在集边界回退（12.04 → 0.08），解码器按回退丢帧：3 集 4/6/3 秒共
//     325 帧，整条 playlist 只解出 275 帧；每片单独解则是满帧；
//   - 把每片 tfdt 平移到「前面各集 EXTINF 之和」后，同一条 playlist 解出 325/325 帧、
//     首 PTS 0.08 / 4.08 / 10.08 严格单调、`ffmpeg -i … -f null -` 无 stderr。
//   - 用 ffmpeg 参数做不到这件事：mov 封装器把 tfdt 归一到轨道起点，
//     `-output_ts_offset` / `-itsoffset` / `-copyts` / `setts` bsf / `+dash` 全部无效
//     （TS 封装则有效，所以旧实现没这个问题）。
//
// 所以时间轴平移放在**出流这一步**做，而不是烘进缓存字节：缓存文件仍然只由「哪一集 +
// 哪个通道」决定（缓存键与 ADR-0011 D3 一字不改），播放位置是呈现层的事。
// 代价是每个非首片请求多一次整片拷贝（12–40 MB），换「改 merge_chunk_size 不产生第二份
// 缓存、也不会因为缓存里躺着旧起点而串味」。

// shiftTimeline 把 MP4 里每个片段的基准解码时间（`moof/traf/tfdt` 的
// baseMediaDecodeTime）加上 offset，返回**新的一段字节**（不改入参：命中缓存时这段字节
// 可能是 single-flight 里多个请求共享的只读缓冲区）。
//
// offset 为 0 时原样返回入参（首片、单集播放都是这一条路径，零拷贝）。
// `tfdt` 的单位是该轨道的媒体 timescale，所以每轨的加数不同：先读 `moov/trak/mdia/mdhd`
// 拿 track_ID → timescale，再按 `tfhd` 的 track_ID 落到对应轨道。
//
// 结构不合规（不是 fMP4、moov 缺失、box 长度越界）返回错误：那是我们自己产出的字节，
// 解析不了说明缓存或生成链路坏了，不能静默当成「平移成功」继续出流。
func shiftTimeline(body []byte, offset time.Duration) ([]byte, error) {
	if offset == 0 {
		return body, nil
	}
	tracks, err := trackTimescales(body)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(body))
	copy(out, body)

	shifted := 0
	for _, b := range mustBoxes(out, 0, len(out)) {
		if b.typ != boxMoof {
			continue
		}
		n, err := shiftMoofBox(out, b, tracks, offset)
		if err != nil {
			return nil, err
		}
		shifted += n
	}
	if shifted == 0 {
		return nil, fmt.Errorf("片段的 moof 里没有 tfdt（不是 fragmented MP4？）")
	}
	return out, nil
}

// shiftMoofBox 把 buf 里一个 moof box 的 tfdt 全部加上 offset，返回改了几处。
//
// buf 既可以是整集字节（shiftTimeline），也可以是单独一块 moof（shiftStream 的
// 增量路径）——b 的坐标按 buf 算，两种调用方式语义相同。
func shiftMoofBox(buf []byte, b box, tracks map[uint32]uint32, offset time.Duration) (int, error) {
	shifted := 0
	for _, traf := range mustBoxes(buf, b.payload, b.end) {
		if traf.typ != boxTraf {
			continue
		}
		var trackID uint32
		for _, child := range mustBoxes(buf, traf.payload, traf.end) {
			if child.typ == boxTfhd {
				// tfhd 是 full box：version/flags(4) 之后才是 track_ID。
				if child.payload+8 > child.end {
					return 0, fmt.Errorf("tfhd 长度不足（%d）", child.end-child.payload)
				}
				trackID = binary.BigEndian.Uint32(buf[child.payload+4:])
			}
		}
		timescale, ok := tracks[trackID]
		if !ok {
			return 0, fmt.Errorf("tfhd 的 track_id=%d 在 moov 里没有对应轨道", trackID)
		}
		ticks := durationTicks(offset, timescale)
		for _, child := range mustBoxes(buf, traf.payload, traf.end) {
			if child.typ != boxTfdt {
				continue
			}
			if err := addTicks(buf, child, ticks); err != nil {
				return 0, err
			}
			shifted++
		}
	}
	return shifted, nil
}

// durationTicks 把时长换算成某个 timescale 下的刻度数（四舍五入）。
func durationTicks(d time.Duration, timescale uint32) uint64 {
	return uint64(d.Seconds()*float64(timescale) + 0.5)
}

// addTicks 把 ticks 加到一段 tfdt 的 baseMediaDecodeTime 上（version 0 是 32 位、
// version 1 是 64 位；两种都可能出现在我们的产物里）。
func addTicks(out []byte, tfdt box, ticks uint64) error {
	if tfdt.payload+4 > tfdt.end {
		return fmt.Errorf("tfdt 长度不足（%d）", tfdt.end-tfdt.payload)
	}
	switch out[tfdt.payload] {
	case 0:
		if tfdt.payload+8 > tfdt.end {
			return fmt.Errorf("tfdt v0 长度不足（%d）", tfdt.end-tfdt.payload)
		}
		cur := binary.BigEndian.Uint32(out[tfdt.payload+4:])
		binary.BigEndian.PutUint32(out[tfdt.payload+4:], cur+uint32(ticks))
	case 1:
		if tfdt.payload+12 > tfdt.end {
			return fmt.Errorf("tfdt v1 长度不足（%d）", tfdt.end-tfdt.payload)
		}
		cur := binary.BigEndian.Uint64(out[tfdt.payload+4:])
		binary.BigEndian.PutUint64(out[tfdt.payload+4:], cur+ticks)
	default:
		return fmt.Errorf("tfdt 版本 %d 不认识", out[tfdt.payload])
	}
	return nil
}

// trackTimescales 读 moov，返回 track_ID → 媒体 timescale（mdhd.timescale）。
func trackTimescales(body []byte) (map[uint32]uint32, error) {
	out := make(map[uint32]uint32, 2)
	seenMoov := false
	for _, b := range mustBoxes(body, 0, len(body)) {
		if b.typ != boxMoov {
			continue
		}
		seenMoov = true
		for _, trak := range mustBoxes(body, b.payload, b.end) {
			if trak.typ != boxTrak {
				continue
			}
			var trackID, timescale uint32
			for _, child := range mustBoxes(body, trak.payload, trak.end) {
				switch child.typ {
				case boxTkhd:
					id, err := readTkhdTrackID(body, child)
					if err != nil {
						return nil, err
					}
					trackID = id
				case boxMdia:
					for _, sub := range mustBoxes(body, child.payload, child.end) {
						if sub.typ != boxMdhd {
							continue
						}
						ts, err := readMdhdTimescale(body, sub)
						if err != nil {
							return nil, err
						}
						timescale = ts
					}
				}
			}
			if trackID != 0 && timescale != 0 {
				out[trackID] = timescale
			}
		}
	}
	if !seenMoov {
		return nil, fmt.Errorf("片段里没有 moov（自包含 fMP4 必有）")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("moov 里没有可用的轨道 timescale")
	}
	return out, nil
}

// readTkhdTrackID 取 tkhd 的 track_ID：v0 的 creation/modification 各 4 字节，
// v1 各 8 字节，之后都是 4 字节 track_ID（ISO/IEC 14496-12 §8.3.2）。
func readTkhdTrackID(body []byte, b box) (uint32, error) {
	if b.payload+4 > b.end {
		return 0, fmt.Errorf("tkhd 长度不足")
	}
	off := b.payload + 4
	switch body[b.payload] {
	case 0:
		off += 8
	case 1:
		off += 16
	default:
		return 0, fmt.Errorf("tkhd 版本 %d 不认识", body[b.payload])
	}
	if off+4 > b.end {
		return 0, fmt.Errorf("tkhd 长度不足（track_ID 越界）")
	}
	return binary.BigEndian.Uint32(body[off:]), nil
}

// readMdhdTimescale 取 mdhd 的 timescale（布局与 tkhd 的 v0/v1 相同，只是
// creation/modification 之后是 timescale 而不是 track_ID）。
func readMdhdTimescale(body []byte, b box) (uint32, error) {
	if b.payload+4 > b.end {
		return 0, fmt.Errorf("mdhd 长度不足")
	}
	off := b.payload + 4
	switch body[b.payload] {
	case 0:
		off += 8
	case 1:
		off += 16
	default:
		return 0, fmt.Errorf("mdhd 版本 %d 不认识", body[b.payload])
	}
	if off+4 > b.end {
		return 0, fmt.Errorf("mdhd 长度不足（timescale 越界）")
	}
	return binary.BigEndian.Uint32(body[off:]), nil
}

// box 是 MP4 的一个 box：typ 是四字符类型，payload 是头之后的第一个字节，end 是末字节后一位。
type box struct {
	typ          string
	payload, end int
}

const (
	boxMoov = "moov"
	boxTrak = "trak"
	boxTkhd = "tkhd"
	boxMdia = "mdia"
	boxMdhd = "mdhd"
	boxMoof = "moof"
	boxTraf = "traf"
	boxTfhd = "tfhd"
	boxTfdt = "tfdt"
)

// mustBoxes 列出 [from, to) 里的顶层 box。长度越界或 size<header 时**截断返回**：
// 调用方都在同一个函数里先做结构校验（trackTimescales 会因为没有 moov/timescale 报错，
// shiftTimeline 会因为 tfdt 数量为 0 报错），所以这里不再各自套一层错误分支。
func mustBoxes(body []byte, from, to int) []box {
	out := make([]box, 0, 8)
	for i := from; i+8 <= to; {
		size := int(binary.BigEndian.Uint32(body[i:]))
		typ := string(body[i+4 : i+8])
		header := 8
		switch size {
		case 0: // 直到文件末尾
			size = to - i
		case 1: // 64 位 largesize
			if i+16 > to {
				return out
			}
			size = int(binary.BigEndian.Uint64(body[i+8:]))
			header = 16
		}
		if size < header || i+size > to {
			return out
		}
		out = append(out, box{typ: typ, payload: i + header, end: i + size})
		i += size
	}
	return out
}
