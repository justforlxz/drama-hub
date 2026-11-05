package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/justforlxz/drama-hub/internal/store"
)

// WriteFunc 把一份缓存内容**边生成边写**：写进 w 的字节立刻可见于已连接的读者，
// 不必等整份内容生成完（MKACG-686：冷启动首片等待从「整集 remux 完成」降到
// 「首个分片落盘」）。
//
// 与 GenerateFunc 的分工：一次成型的对象（海报）用 GenerateFunc；分片这种
// 「第一个字节就能播」的流式对象用 WriteFunc。
type WriteFunc func(ctx context.Context, w io.Writer) error

// Stream 是一份缓存内容的读视图：要么已经完整落盘（Cached），要么正在生成
// （Size 之后还会增长）。读者只面对 Read/ReadFrom，不关心生成细节。
//
// 生命周期：GetOrStream 返回后立即可读；生成完成时文件从 tmp/ rename 到内容目录
// 并写 artifact 行，已打开的读者句柄仍指向同一个 inode，不受 rename 影响。
type Stream struct {
	spec       Spec
	target     string
	prefetched bool
	cache      *Cache
	buildID    string

	file        *os.File // 生成侧持有的临时文件句柄
	ctx         context.Context
	gen         WriteFunc
	cancelBuild context.CancelFunc

	mu      sync.Mutex
	path    string // 可读路径：生成中是 tmp 里的半成品，完成后是最终文件
	n       int64  // 已落盘、读者可读到的字节数
	err     error
	done    bool
	played  bool // 有读者拿过字节：落 artifact 行时不标 prefetched
	readers int
	cached  bool
	wake    chan struct{} // 每次增长/完成时关闭并换新的（广播）

	finished chan struct{}
}

// Cached 报告这份内容在本次请求之前就已经完整落盘（命中）。
func (s *Stream) Cached() bool { return s.cached }

// Size 返回已落盘字节数（生成中会增长）。
func (s *Stream) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// Err 返回生成结果：nil 表示成功（或还在生成中）。
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Bytes 返回完整内容（调用前应确认 Cached 或 Wait 已成功）。
func (s *Stream) Bytes() ([]byte, error) {
	s.mu.Lock()
	path, done, err := s.path, s.done, s.err
	s.mu.Unlock()
	if !done {
		return nil, errors.New("内容还没生成完，不能整读")
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Wait 等到生成结束（成功或失败）。ctx 取消时放弃等待，并在没有读者时停掉生成：
// 没人要的字节不该继续占着 ffmpeg 闸。
func (s *Stream) Wait(ctx context.Context) error {
	select {
	case <-s.finished:
		return s.Err()
	case <-ctx.Done():
		s.abortIfIdle()
		return ctx.Err()
	}
}

// Reader 从 off 开始读这份内容：读到当前末尾就等增长，生成结束后返回 io.EOF
// （生成失败则把错误交给调用方，读者据此断开响应）。
func (s *Stream) Reader(ctx context.Context, off int64) (io.ReadCloser, error) {
	s.mu.Lock()
	if off < 0 || off > s.n {
		n := s.n
		s.mu.Unlock()
		return nil, fmt.Errorf("读取偏移 %d 超出已生成范围 %d", off, n)
	}
	if s.done && s.err != nil && off == s.n {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	// 开文件与切换读路径同锁：领跑者在 rename 与 finish 之间有一个窗口，那时
	// tmp 已经不存在、s.path 还没指向最终文件；如果读者在锁外取路径再 open，就会
	// 打开一个刚被搬走的半成品，冷路径同集并发播放时后到者直接 500
	// （MKACG-686 代码审查 blocker）。锁内 open 保证拿到的路径此刻一定存在。
	path := s.path
	f, err := os.Open(path)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("读缓存文件 %s: %w", path, err)
	}
	s.readers++
	s.played = true
	s.mu.Unlock()
	return &streamReader{s: s, ctx: ctx, off: off, f: f}, nil
}

// streamReader 是 Stream 的读句柄：阻塞等新字节，不持有生成侧的状态。
type streamReader struct {
	s   *Stream
	ctx context.Context
	off int64
	f   *os.File
}

func (r *streamReader) Read(p []byte) (int, error) {
	for {
		n, done, wake, err := r.s.snapshot()
		if r.off < n {
			want := len(p)
			if int64(want) > n-r.off {
				want = int(n - r.off)
			}
			got, rerr := r.f.ReadAt(p[:want], r.off)
			if got > 0 {
				r.off += int64(got)
				return got, nil
			}
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return 0, fmt.Errorf("读缓存文件: %w", rerr)
			}
			continue
		}
		if done {
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		select {
		case <-wake:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

// Close 释放读句柄；最后一个读者离开且生成还没结束就停掉生成（没人要了）。
func (r *streamReader) Close() error {
	r.s.detach()
	return r.f.Close()
}

func (s *Stream) snapshot() (n int64, done bool, wake chan struct{}, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n, s.done, s.wake, s.err
}

// grow 把已写入的字节数推给读者并广播。
func (s *Stream) grow(n int64) {
	s.mu.Lock()
	s.n += n
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// detach 释放一个读者；没人读且没生成完就取消生成。
func (s *Stream) detach() {
	s.mu.Lock()
	s.readers--
	idle := s.readers <= 0 && !s.done
	s.mu.Unlock()
	if idle && s.cancelBuild != nil {
		s.cancelBuild()
	}
}

// abortIfIdle 在没有读者时取消生成。
func (s *Stream) abortIfIdle() {
	s.mu.Lock()
	idle := s.readers <= 0 && !s.done
	s.mu.Unlock()
	if idle && s.cancelBuild != nil {
		s.cancelBuild()
	}
}

// GetOrStream 返回一份缓存内容的读视图：命中直接给完整文件；未命中则领跑（或在等
// 同一份正在生成的字节）。生成过程与 Prefetch 共用同一张在途表，因此同一集
// 前台与预热加起来只有一个 ffmpeg（MKACG-686 验收第 5 条）。
func (c *Cache) GetOrStream(ctx context.Context, spec Spec, gen WriteFunc, prefetched bool) (*Stream, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	// 命中：与 GetOrGenerate 一样 touch + 清 prefetched（用户真的播到了这一片）。
	if row, ok, err := c.cached(ctx, spec); err != nil {
		return nil, err
	} else if ok {
		_ = c.st.MarkArtifactPlayed(ctx, spec.Kind, spec.Key, stamp(c.now()))
		return hitStream(spec, row), nil
	}

	id := spec.Kind + keySeparator + spec.Key
	c.buildMu.Lock()
	if s, ok := c.builds[id]; ok {
		if !s.dead() {
			c.buildMu.Unlock()
			return s, nil
		}
		// 领跑者已经失败/被取消：不挂在死掉的生成上，换一个新的（调用方也会重试一次）。
		delete(c.builds, id)
	}
	// 锁内复查缓存：领跑者的「写 artifact 行 → 摘在途表」之间，本请求的第一次查询可能
	// 落在两者之前（查到未命中），此刻若直接新建就会在同一份字节上跑第二个 ffmpeg
	// （MKACG-640 窗口 2 的回归）。复查后「查不到在途、又命中不了缓存」不存在。
	if row, ok, err := c.cached(ctx, spec); err != nil {
		c.buildMu.Unlock()
		return nil, err
	} else if ok {
		c.buildMu.Unlock()
		_ = c.st.MarkArtifactPlayed(ctx, spec.Kind, spec.Key, stamp(c.now()))
		return hitStream(spec, row), nil
	}
	s, err := c.newStream(ctx, spec, gen, prefetched)
	if err != nil {
		c.buildMu.Unlock()
		return nil, err
	}
	c.builds[id] = s
	c.buildMu.Unlock()
	go s.run()
	return s, nil
}

// hitStream 把一条命中的 artifact 行包成已完成的读视图。
func hitStream(spec Spec, row store.Artifact) *Stream {
	return &Stream{
		spec: spec, path: row.Path, n: row.Bytes, done: true, cached: true,
		wake: make(chan struct{}), finished: closedChan(),
	}
}

// newStream 建临时文件与生成上下文。临时文件在返回前就建好，读者不会撞上 ENOENT。
func (c *Cache) newStream(ctx context.Context, spec Spec, gen WriteFunc, prefetched bool) (*Stream, error) {
	root, err := cacheRoot(ctx, c.settings)
	if err != nil {
		return nil, err
	}
	l := layoutOf(root)
	target, err := l.file(spec.Kind, cacheHash(spec.Kind, spec.Key), spec.Ext)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.tmpDir(), 0o755); err != nil {
		return nil, err
	}
	// 半成品名带 .partial：它永远待在 tmp/，不进 artifact 表，因此进程重启后
	// 绝不会被当成完整产物服务（验收第 6 条），淘汰时按 tmp 规则清扫。
	f, err := os.CreateTemp(l.tmpDir(), ".partial-*")
	if err != nil {
		return nil, err
	}
	// 生成跟着调用方走：前台断开、预热被 preempt 都会立刻停掉 ffmpeg（不必等满
	// remux 超时）。等这份字节的读者如果还一个字节都没拿到，由调用方重新领跑一次
	// （GetOrGenerate / ServeSegment 的重试，与旧实现「等待者接棒」等价）。
	bctx, cancel := context.WithCancel(ctx)
	return &Stream{
		spec: spec, target: target, prefetched: prefetched,
		cache: c, buildID: spec.Kind + keySeparator + spec.Key,
		file: f, ctx: bctx, gen: gen, cancelBuild: cancel,
		path: f.Name(), wake: make(chan struct{}), finished: make(chan struct{}),
	}, nil
}

// run 是领跑者：生成 → 原子落盘 → 写 artifact 行，全程广播进度。
func (s *Stream) run() {
	tmp := s.file.Name()
	id := s.buildID

	h := sha256.New()
	sink := &streamSink{s: s, w: io.MultiWriter(s.file, h)}

	err := s.gen(s.ctx, sink)
	if cerr := s.file.Close(); err == nil {
		err = cerr
	}
	if err == nil && s.Size() == 0 {
		err = fmt.Errorf("生成 %s/%s 的内容为空", s.spec.Kind, s.spec.Key)
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	// rename 与读路径切换同锁：否则「tmp 已搬走、s.path 还没指向最终文件」的窗口里
	// 新读者会 open 到不存在的半成品（MKACG-686 代码审查 blocker）。
	s.mu.Lock()
	if err == nil {
		if rerr := os.Rename(tmp, s.target); rerr != nil {
			err = rerr
		} else {
			s.path = s.target
		}
	}
	played := s.played
	s.mu.Unlock()
	if err == nil {
		err = s.cache.st.PutArtifact(context.WithoutCancel(s.ctx), store.Artifact{
			Kind:       s.spec.Kind,
			Key:        s.spec.Key,
			Path:       s.target,
			Bytes:      s.Size(),
			SHA256:     hex.EncodeToString(h.Sum(nil)),
			LastUsedAt: stamp(s.cache.now()),
			Prefetched: s.prefetched && !played,
		})
		if err != nil {
			_ = os.Remove(s.target)
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	// 先摘除在途表再发布结果：失败的领跑者不该再被晚到者挂上（成功的那个在摘除时
	// artifact 行已经写好，晚到者走缓存命中，不存在「两边都没有」的窗口）。
	s.cache.buildMu.Lock()
	if s.cache.builds[id] == s {
		delete(s.cache.builds, id)
	}
	s.cache.buildMu.Unlock()
	s.finish(err)
}

// dead 报告这次生成已经失败结束（成功的由 artifact 行兜住，不进这个判断）。
func (s *Stream) dead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done && s.err != nil
}

// finish 发布最终状态。读路径在 run 里随 rename 一起切好，这里不再动它。
func (s *Stream) finish(err error) {
	s.mu.Lock()
	s.done = true
	s.err = err
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
	close(s.finished)
}

// streamSink 把生成侧写出的字节同时记到缓存文件与摘要里，并广播给读者。
type streamSink struct {
	s *Stream
	w io.Writer
}

func (w *streamSink) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.s.grow(int64(n))
	}
	return n, err
}

// closedChan 返回一个已关闭的 channel（命中路径的 finished）。
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
