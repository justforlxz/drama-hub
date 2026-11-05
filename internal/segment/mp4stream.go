package segment

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// 增量版的时间轴平移（MKACG-686）
//
// 冷启动要边生成边出流，就不能像 shiftTimeline 那样等整集字节齐了再拷一份。
// 这里按顶层 box 走：ftyp / mdat 之类的直通，moov 到了就先读轨道 timescale，
// moof 整块缓冲、原地改 tfdt 再放出。输出必须与 shiftTimeline 逐字节一致
// （单测 shiftStream_matchesShiftTimeline 用任意切分钉住）。
//
// 为什么 moof 必须整块缓冲：tfdt 在 moof 内部，box 头里的 size 决定后面还有多少
// 字节，攒齐才能保证「要么整块放出去、要么一个字节都不放」——半块 moof 客户端解不了。

// streamChunk 是直通 box（mdat 等）单次搬运的上限：够大以摊薄系统调用，够小以
// 让首个分片尽快离开内存。
const streamChunk = 64 << 10

// shiftStream 返回把 fMP4 时间轴平移 offset 的读流。offset 为 0 时原样透传
// （首片、单集播放走这条路，零解析零拷贝）。
func shiftStream(r io.Reader, offset time.Duration) io.Reader {
	if offset == 0 {
		return r
	}
	return &shiftStreamReader{r: r, offset: offset}
}

type shiftStreamReader struct {
	r      io.Reader
	offset time.Duration
	tracks map[uint32]uint32

	pending []byte
	err     error
	// remain 是当前直通 box 还没搬的字节数；toEOF 表示 size==0（直到流末尾）。
	remain int64
	toEOF  bool
}

func (s *shiftStreamReader) Read(p []byte) (int, error) {
	for {
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			return n, nil
		}
		if s.err != nil {
			return 0, s.err
		}
		if err := s.fill(); err != nil {
			s.err = err
		}
	}
}

// fill 读一小段源流，把「可以放出去的字节」放进 pending（至少 1 字节，除非出错）。
func (s *shiftStreamReader) fill() error {
	if s.toEOF {
		buf := make([]byte, streamChunk)
		n, err := s.r.Read(buf)
		if n > 0 {
			s.pending = append(s.pending, buf[:n]...)
			return nil
		}
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return err
	}
	if s.remain > 0 {
		want := int64(streamChunk)
		if s.remain < want {
			want = s.remain
		}
		buf := make([]byte, want)
		if _, err := io.ReadFull(s.r, buf); err != nil {
			return fmt.Errorf("读分片内容: %w", err)
		}
		s.remain -= want
		s.pending = append(s.pending, buf...)
		return nil
	}

	head := make([]byte, 8)
	if _, err := io.ReadFull(s.r, head); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("读分片 box 头: %w", err)
	}
	size := int64(binary.BigEndian.Uint32(head))
	typ := string(head[4:8])
	headerLen := int64(8)
	if size == 1 {
		large := make([]byte, 8)
		if _, err := io.ReadFull(s.r, large); err != nil {
			return fmt.Errorf("读分片 box largesize: %w", err)
		}
		size = int64(binary.BigEndian.Uint64(large))
		head = append(head, large...)
		headerLen = 16
	}
	switch size {
	case 0:
		// 直到流末尾：只可能是最后一个 box，直通。
		s.pending = append(s.pending, head...)
		s.toEOF = true
		return nil
	}
	if size < headerLen {
		return fmt.Errorf("分片 box %q 的长度 %d 不合法", typ, size)
	}

	if typ != boxMoov && typ != boxMoof {
		// ftyp / mdat / free / sidx ……：头先出，内容按块搬，不在内存里攒整块
		// （mdat 可以到几十 MB）。
		s.pending = append(s.pending, head...)
		s.remain = size - headerLen
		return nil
	}

	body := make([]byte, size)
	copy(body, head)
	if _, err := io.ReadFull(s.r, body[headerLen:]); err != nil {
		return fmt.Errorf("读分片 box %q: %w", typ, err)
	}
	if typ == boxMoov {
		tracks, err := trackTimescales(body)
		if err != nil {
			return err
		}
		s.tracks = tracks
		s.pending = append(s.pending, body...)
		return nil
	}

	b := box{typ: boxMoof, payload: int(headerLen), end: int(size)}
	shifted, err := shiftMoofBox(body, b, s.tracks, s.offset)
	if err != nil {
		return err
	}
	if shifted == 0 {
		return fmt.Errorf("片段的 moof 里没有 tfdt（不是 fragmented MP4？）")
	}
	s.pending = append(s.pending, body...)
	return nil
}
