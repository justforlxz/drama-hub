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
	"sync"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// ErrUnsupportedKind 表示要缓存的对象不在契约里：只有海报 / 分片 / 关键帧索引三类
// （docs/CACHE-MODEL.md「缓存什么、不缓存什么」）。m3u8 文本与源整片没有对应的 kind，
// 因此写不进缓存。
var ErrUnsupportedKind = errors.New("不支持的缓存类型")

// Spec 是一次缓存读写的位置：kind 决定内容目录与允许的后缀，key 是契约键（见
// keys.go），ext 是落盘后缀。组合不合法时 GetOrGenerate 直接报错、不落盘。
type Spec struct {
	Kind string // store.ArtifactKindSegment | ArtifactKindIndex | ArtifactKindCover
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
		if s.Ext != ".ts" {
			return fmt.Errorf("%w: 分片后缀必须是 .ts，实际 %q", ErrUnsupportedKind, s.Ext)
		}
		if !isSHA256Hex(s.Key) {
			// 分片的键就是落盘文件名，只能由 SegmentKey 产出（64 位十六进制）；
			// 放任意字符串进来会写出路径里没法分桶、也没法和别人对齐的文件。
			return fmt.Errorf("%w: 分片键必须是 sha256 十六进制，实际 %q", ErrUnsupportedKind, s.Key)
		}
	case store.ArtifactKindIndex:
		if s.Ext != ".json" {
			return fmt.Errorf("%w: 索引后缀必须是 .json，实际 %q", ErrUnsupportedKind, s.Ext)
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

// GenerateFunc 生成一份缓存内容（一次 ffmpeg 切片、一次 ffprobe 建索引……）。
//
// 失败必须原样返回错误：本包不吞错误也不写缓存，边界包据此映射状态码
// （生成失败 → 502，且目录与 artifact 都不变 —— docs/CACHE-MODEL.md「命中后的读取路径」）。
type GenerateFunc func(ctx context.Context) ([]byte, error)

// Cache 是本地缓存（海报 / 分片 / 关键帧索引）的入口：命中即回本地字节并 touch，
// 未命中走 single-flight 生成 → 原子落盘 → 写 artifact 行。淘汰见 evict.go。
//
// 不缓存 artifact 行、不缓存 settings：每次都读库，改完即时生效（硬约束 5）。
type Cache struct {
	st       *store.Store
	settings *store.Settings
	logger   *slog.Logger
	now      func() time.Time // 取当前时间；测试注入固定值（last_used_at 与淘汰要可断言）

	mu      sync.Mutex // 只保护 flights；goroutine 之间不共享别的可变状态
	flights map[string]*flight

	hooks hooks // 测试注入的单飞观察点；生产为 nil，见 hooks 的注释
}

// New 构造缓存。st / settings / logger 由装配根注入，不可为 nil。
func New(st *store.Store, settings *store.Settings, logger *slog.Logger) *Cache {
	return &Cache{
		st:       st,
		settings: settings,
		logger:   logger,
		now:      time.Now,
		flights:  make(map[string]*flight),
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
	return c.singleflight(ctx, spec.Kind+keySeparator+spec.Key, func() ([]byte, error) {
		// 当上 leader 后再查一次：从上面的 hit() 到这里之间，可能有另一个 leader
		// 已经生成完并摘掉了 flight（发布先于摘除，所以那时 hit() 必然命中）。
		// 不复查就会成为第二个 leader，同一片被切两遍 —— MKACG-640。
		if body, ok, err := c.hit(ctx, spec); err != nil || ok {
			return body, err
		}
		return c.generate(ctx, spec, gen)
	})
}

// hit 查索引行并读文件：都成功才算命中，并把 last_used_at 推到现在。
//
// 行在、文件没了（被手删，或上一轮淘汰删了文件没删成行）按未命中处理，并顺手删掉
// 这条脏行：淘汰用 SUM(bytes) 算总量，留着指向不存在文件的字节会虚高。
func (c *Cache) hit(ctx context.Context, spec Spec) (body []byte, ok bool, err error) {
	row, err := c.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	body, err = os.ReadFile(row.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if derr := c.st.DeleteArtifact(ctx, spec.Kind, spec.Key); derr != nil &&
				!errors.Is(derr, store.ErrNotFound) {
				return nil, false, derr
			}
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读缓存文件 %s: %w", row.Path, err)
	}
	// 命中即 touch（契约 MUST）：不 touch 的条目会被 cache_lru_days 当成过期删掉，
	// 而它天天在用。touch 失败只让这条更早被淘汰，不影响这次正确性，因此忽略。
	_ = c.st.TouchArtifact(ctx, spec.Kind, spec.Key, stamp(c.now()))
	return body, true, nil
}

// generate 生成 → 落盘 → 写行。任一步失败都不留缓存：没 rename 到位就不写行；
// 写行失败就把刚落的文件删掉——否则它既不会被命中，也不会被淘汰扫到（淘汰按行驱动）。
func (c *Cache) generate(ctx context.Context, spec Spec, gen GenerateFunc) ([]byte, error) {
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
	}); err != nil {
		_ = os.Remove(target)
		return nil, err
	}
	return body, nil
}

// flight 是一次进行中的生成：leader 写完 body / err 后 close(done)，等待者读同一份。
type flight struct {
	done chan struct{}
	body []byte
	err  error
}

// hooks 是单飞状态迁移的观察点，只给测试用：测试在 New 之后设 c.hooks 卡住某个瞬间，
// 把本来要靠调度运气才撞上的窗口变成确定性的。两个字段为 nil（生产的唯一取值）时
// GetOrGenerate / singleflight 里各只剩一次 nil 判断。
type hooks struct {
	// beforeEnter 在「初次命中选择失败、即将进入单飞」之间调用，调用时不持锁。
	beforeEnter func()

	// published 在 leader 的结果已发布（close(done) 已完成）、flight 尚未摘除时调用，
	// 调用时持有 c.mu：钩子里只能做只读断言（或调 c.hit —— 它不碰 c.mu），
	// 回调 GetOrGenerate 会重入同一把锁而死锁。
	published func(id string)
}

// singleflight 把同一 id 的并发生成合成一次：先到的是 leader，其余等它的结果。
//
// mutex 只保护 map（与 channel 分工，不混用）。leader 的 ctx 决定生成是否继续：
// 它被取消时等待者拿到同一个错误，各自的请求自己重试 —— 比在这里做生成的生命周期
// 管理便宜，也不会留下无人接收的结果。
func (c *Cache) singleflight(ctx context.Context, id string, gen func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if f, ok := c.flights[id]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.body, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[id] = f
	c.mu.Unlock()

	// 先赋值再 close：close 建立 happens-before，等待者看不到半写的 body/err。
	f.body, f.err = gen()

	// 发布与摘除在同一把锁内，且发布在前：晚到者要么还能在 map 里看到这个 flight、
	// 等 done（结果已经可用，不重生成），要么看到它已消失 —— 而「已消失」意味着结果
	// 已发布，那时 hit() 必然命中。反过来（先摘除、解锁后再 close）会留出一个
	// 「map 里没有、结果又还没发布」的窗口，晚到者在窗口里当上第二个 leader，
	// 同一个分片被切两遍 —— MKACG-640 的 10 并发用例就是这么红的。
	c.mu.Lock()
	close(f.done)
	if h := c.hooks.published; h != nil {
		h(id)
	}
	delete(c.flights, id)
	c.mu.Unlock()
	return f.body, f.err
}

// stamp 是 artifact.last_used_at 的统一格式：UTC RFC3339（定长、可按字符串排序）。
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
