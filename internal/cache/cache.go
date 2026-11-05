package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// ErrUnsupportedKind 表示要缓存的对象不在契约里：只有海报与分片两类
// （docs/CACHE-MODEL.md「缓存什么、不缓存什么」）。m3u8 文本与源整片没有对应的 kind，
// 因此写不进缓存。
var ErrUnsupportedKind = errors.New("不支持的缓存类型")

// Spec 是一次缓存读写的位置：kind 决定内容目录与允许的后缀，key 是契约键（见
// keys.go），ext 是落盘后缀。组合不合法时 GetOrGenerate 直接报错、不落盘。
type Spec struct {
	Kind string // store.ArtifactKindSegment | ArtifactKindCover
	Key  string
	Ext  string
}

// validate 钉住「kind → 目录 + 后缀」的一一对应：正文不能写进别的树，m3u8 也不能
// 冒充分片后缀塞进来（m3u8 文本按契约不缓存）。
func (s Spec) validate() error {
	if s.Key == "" {
		return errors.New("缓存键为空")
	}
	switch s.Kind {
	case store.ArtifactKindSegment:
		if s.Ext != ".m4s" {
			return fmt.Errorf("%w: 分片后缀必须是 .m4s，实际 %q", ErrUnsupportedKind, s.Ext)
		}
		if !isSHA256Hex(s.Key) {
			// 分片的键就是落盘文件名，只能由 SegmentKey 产出（64 位十六进制）；
			// 放任意字符串进来会写出路径里没法分桶、也没法和别人对齐的文件。
			return fmt.Errorf("%w: 分片键必须是 sha256 十六进制，实际 %q", ErrUnsupportedKind, s.Key)
		}
	case store.ArtifactKindCover:
		if !imageExt(s.Ext) {
			return fmt.Errorf("%w: 海报后缀 %q 不是图片", ErrUnsupportedKind, s.Ext)
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedKind, s.Kind)
	}
	return nil
}

// isSHA256Hex 判定 key 是否是 64 位小写十六进制（sha256 的十六进制形态）。
func isSHA256Hex(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// imageExt 判定海报后缀（取值集合同 coverExt）。
func imageExt(ext string) bool {
	switch ext {
	case ".jpg", ".png", ".webp", ".gif", ".img":
		return true
	}
	return false
}

// GenerateFunc 生成一份缓存内容（一次整集 ffmpeg remux、一次海报下载……）。
//
// 失败必须原样返回错误：本包不吞错误也不写缓存，边界包据此映射状态码
// （生成失败 → 502，且目录与 artifact 都不变 —— docs/CACHE-MODEL.md「命中后的读取路径」）。
type GenerateFunc func(ctx context.Context) ([]byte, error)

// Cache 是本地缓存（海报 / 分片）的入口：命中即回本地字节并 touch，
// 未命中走 single-flight 生成 → 原子落盘 → 写 artifact 行。淘汰见 evict.go。
//
// 不缓存 artifact 行、不缓存 settings：每次都读库，改完即时生效（硬约束 5）。
type Cache struct {
	st       *store.Store
	settings *store.Settings
	logger   *slog.Logger
	now      func() time.Time // 取当前时间；测试注入固定值（last_used_at 与淘汰要可断言）

	// builds 是在途的流式生成（GetOrStream / Prefetch 共用）：同一个 (kind, key)
	// 只有一个领跑者，读者与预热都挂在它上面，加起来只跑一个 ffmpeg。
	buildMu sync.Mutex
	builds  map[string]*Stream
}

// New 构造缓存。st / settings / logger 由装配根注入，不可为 nil。
func New(st *store.Store, settings *store.Settings, logger *slog.Logger) *Cache {
	return &Cache{
		st:       st,
		settings: settings,
		logger:   logger,
		now:      time.Now,
		builds:   make(map[string]*Stream),
	}
}

// GetOrGenerate 返回一份缓存内容的完整字节（调用方要整块用，例如海报）。
//
// 与 GetOrStream 共用同一套在途表与落盘逻辑：命中直接读本地文件（并 touch、清掉
// prefetched 标记），未命中生成、落盘并写 artifact 行。整块取用的调用方不需要
// 「边生成边读」，所以这里就是「挂成读者读到底」。
func (c *Cache) GetOrGenerate(ctx context.Context, spec Spec, gen GenerateFunc) ([]byte, error) {
	body, err := c.readAll(ctx, spec, gen)
	// 领跑者被取消（前台 preempt 掉预热等）：自己重新领跑一次。这不是内容错误，
	// 不能映射成 502（MKACG-682 代码审查 blocker）。调用方自己取消时不重试。
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return c.readAll(ctx, spec, gen)
	}
	return body, err
}

// readAll 是 GetOrGenerate 的一次尝试：挂成读者把整份内容读到底。
func (c *Cache) readAll(ctx context.Context, spec Spec, gen GenerateFunc) ([]byte, error) {
	s, err := c.GetOrStream(ctx, spec, func(ctx context.Context, w io.Writer) error {
		body, err := gen(ctx)
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}, false)
	if err != nil {
		return nil, err
	}
	if s.Cached() {
		return s.Bytes()
	}
	rd, err := s.Reader(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

// cached 确认一份缓存是否已经有本地字节：行在、文件在才算有。不读文件内容，也不
// 更新 last_used_at（预热的检查用它，不该把没播过的条目「用」一次）。
func (c *Cache) cached(ctx context.Context, spec Spec) (store.Artifact, bool, error) {
	row, err := c.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if errors.Is(err, store.ErrNotFound) {
		return store.Artifact{}, false, nil
	}
	if err != nil {
		return store.Artifact{}, false, err
	}
	if _, err := os.Stat(row.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if derr := c.st.DeleteArtifact(ctx, spec.Kind, spec.Key); derr != nil &&
				!errors.Is(derr, store.ErrNotFound) {
				return store.Artifact{}, false, derr
			}
			return store.Artifact{}, false, nil
		}
		return store.Artifact{}, false, fmt.Errorf("读缓存文件 %s: %w", row.Path, err)
	}
	return row, true, nil
}

// Prefetch 在后台把一份内容生成并落进缓存，标记成 prefetched（MKACG-682）。
//
// 与 GetOrGenerate 的区别只有两点：已经有本地字节就直接返回（不重复生成、不读回
// 整片、不改标记）；生成时写 prefetched=true，容量吃紧时这类分片先被淘汰。
// 共用同一组 single-flight：前台正在生成同一个键时，预热等它的结果，不会重复跑
// ffmpeg；反过来预热当 leader 时，前台等待者拿到同一份字节。
//
// 不返回字节：预热的结果不进当前请求。失败原样返回错误，调用方只记日志。
func (c *Cache) Prefetch(ctx context.Context, spec Spec, gen WriteFunc) error {
	if err := spec.validate(); err != nil {
		return err
	}
	if ok, err := c.Has(ctx, spec); err != nil || ok {
		return err
	}
	// 与前台共用同一张在途表（GetOrStream）：前台正在生成这一集时预热只是等它，
	// 反过来预热领跑时前台读同一份字节 —— 同一集永远只有一个 ffmpeg。
	s, err := c.GetOrStream(ctx, spec, gen, true)
	if err != nil {
		return err
	}
	return s.Wait(ctx)
}

// Has 报告一份缓存是否已有本地字节（不读内容、不动 last_used_at）。
//
// 预热的「已缓存就不重复生成」用它：GetOrGenerate 的命中路径会把条目标记成「播过」，
// 预热不该借一次检查把没播过的分片洗白。
func (c *Cache) Has(ctx context.Context, spec Spec) (bool, error) {
	if err := spec.validate(); err != nil {
		return false, err
	}
	_, ok, err := c.cached(ctx, spec)
	return ok, err
}

// stamp 是 artifact.last_used_at 的统一格式：UTC RFC3339（定长、可按字符串排序）。
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
