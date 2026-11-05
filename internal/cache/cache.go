package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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

	flights *flightGroup // single-flight：同一 (kind, key) 的并发生成只做一次

	hooks hooks // 测试注入的单飞观察点；生产为 nil，见 hooks 的注释
}

// New 构造缓存。st / settings / logger 由装配根注入，不可为 nil。
func New(st *store.Store, settings *store.Settings, logger *slog.Logger) *Cache {
	return &Cache{
		st:       st,
		settings: settings,
		logger:   logger,
		now:      time.Now,
		flights:  newFlightGroup(),
	}
}

// GetOrGenerate 返回一份缓存内容的完整字节：命中直接读本地文件，未命中调 gen 生成、
// 落盘并写 artifact 行。
//
// 返回完整字节（不返回文件句柄）：单片 10 s 约 2–5 MB，边界包用
// http.ServeContent(w, r, name, modTime, bytes.NewReader(body)) 即可处理 Range。
//
// 并发：同一个 (kind, key) 的并发调用只生成一次，其余等同一份结果；不同键互不阻塞。
func (c *Cache) GetOrGenerate(ctx context.Context, spec Spec, gen GenerateFunc) ([]byte, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if body, ok, err := c.hit(ctx, spec); err != nil || ok {
		return body, err
	}
	if h := c.hooks.beforeEnter; h != nil {
		h()
	}
	body, err := c.flights.do(ctx, spec.Kind+keySeparator+spec.Key, func() ([]byte, error) {
		// 当上 leader 后再查一次：从上面的 hit() 到这里之间，可能有另一个 leader
		// 已经生成完并摘掉了 flight（发布先于摘除，所以那时 hit() 必然命中）。
		// 不复查就会成为第二个 leader，同一片被切两遍 —— MKACG-640。
		if body, ok, err := c.hit(ctx, spec); err != nil || ok {
			return body, err
		}
		return c.generate(ctx, spec, gen, false)
	}, c.hooks.published)
	if err != nil {
		return nil, err
	}
	// 这次结果可能来自别人领跑的 flight（前台正好等到预热）：那条行写的是
	// prefetched=true，可前台已经把字节交给播放端了。补一次标记，否则「用户播过」
	// 会在容量吃紧时被当成「没播过」优先淘汰（MKACG-682 代码审查 nit）。
	_ = c.st.MarkArtifactPlayed(ctx, spec.Kind, spec.Key, stamp(c.now()))
	return body, nil
}

// hit 查索引行并读文件：都成功才算命中，并把 last_used_at 推到现在、清掉预热标记。
//
// 行在、文件没了（被手删，或上一轮淘汰删了文件没删成行）按未命中处理，并顺手删掉
// 这条脏行：淘汰用 SUM(bytes) 算总量，留着指向不存在文件的字节会虚高。
func (c *Cache) hit(ctx context.Context, spec Spec) (body []byte, ok bool, err error) {
	row, ok, err := c.cached(ctx, spec)
	if err != nil || !ok {
		return nil, false, err
	}
	body, err = os.ReadFile(row.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 与 cached 的判断之间刚被淘汰：删掉脏行，按未命中处理。
			if derr := c.st.DeleteArtifact(ctx, spec.Kind, spec.Key); derr != nil &&
				!errors.Is(derr, store.ErrNotFound) {
				return nil, false, derr
			}
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读缓存文件 %s: %w", row.Path, err)
	}
	// 命中即 touch（契约 MUST）：不 touch 的条目会被 cache_lru_days 当成过期删掉，
	// 而它天天在用。同时清掉 prefetched —— 用户真的播到了这一片，容量吃紧时它不再
	// 是「预热优先淘汰」的候选。失败只影响淘汰顺序，不影响这次正确性，因此忽略。
	_ = c.st.MarkArtifactPlayed(ctx, spec.Kind, spec.Key, stamp(c.now()))
	return body, true, nil
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
func (c *Cache) Prefetch(ctx context.Context, spec Spec, gen GenerateFunc) error {
	if err := spec.validate(); err != nil {
		return err
	}
	if ok, err := c.Has(ctx, spec); err != nil || ok {
		return err
	}
	_, err := c.flights.do(ctx, spec.Kind+keySeparator+spec.Key, func() ([]byte, error) {
		// 与 GetOrGenerate 同样的领跑复查：前台可能刚生成完并摘掉 flight，
		// 不复查就会把同一片再切一遍（预热白跑一次 ffmpeg）。
		if ok, err := c.Has(ctx, spec); err != nil || ok {
			return nil, err
		}
		return c.generate(ctx, spec, gen, true)
	}, c.hooks.published)
	return err
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

// generate 生成 → 落盘 → 写行。任一步失败都不留缓存：没 rename 到位就不写行；
// 写行失败就把刚落的文件删掉——否则它既不会被命中，也不会被淘汰扫到（淘汰按行驱动）。
//
// prefetched 写进 artifact.prefetched：前台生成是 false，后台预热是 true。
func (c *Cache) generate(ctx context.Context, spec Spec, gen GenerateFunc, prefetched bool) ([]byte, error) {
	body, err := gen(ctx)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("生成 %s/%s 的内容为空", spec.Kind, spec.Key)
	}
	root, err := cacheRoot(ctx, c.settings)
	if err != nil {
		return nil, err
	}
	l := layoutOf(root)
	target, err := l.file(spec.Kind, cacheHash(spec.Kind, spec.Key), spec.Ext)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(l.tmpDir(), target, body); err != nil {
		return nil, fmt.Errorf("写入缓存文件 %s: %w", target, err)
	}
	digest := sha256.Sum256(body)
	if err := c.st.PutArtifact(ctx, store.Artifact{
		Kind:       spec.Kind,
		Key:        spec.Key,
		Path:       target,
		Bytes:      int64(len(body)),
		SHA256:     hex.EncodeToString(digest[:]),
		LastUsedAt: stamp(c.now()),
		Prefetched: prefetched,
	}); err != nil {
		_ = os.Remove(target)
		return nil, err
	}
	return body, nil
}

// hooks 是单飞状态迁移的观察点，只给测试用：测试在 New 之后设 c.hooks 卡住某个瞬间，
// 把本来要靠调度运气才撞上的窗口变成确定性的。两个字段为 nil（生产的唯一取值）时
// GetOrGenerate / flightGroup.do 里各只剩一次 nil 判断。
type hooks struct {
	// beforeEnter 在「初次命中选择失败、即将进入单飞」之间调用，调用时不持锁。
	beforeEnter func()

	// published 在 leader 的结果已发布（close(done) 已完成）、flight 尚未摘除时调用，
	// 调用时持有 flightGroup 的锁：钩子里只能做只读断言（或调 c.hit —— 它不碰那把锁），
	// 回调 GetOrGenerate 会重入同一把锁而死锁。
	published func(id string)
}

// stamp 是 artifact.last_used_at 的统一格式：UTC RFC3339（定长、可按字符串排序）。
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
