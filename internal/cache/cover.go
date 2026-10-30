package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// ErrCoverFetch 表示海报取不回来（非 2xx、读体失败、传输错误）。
// 调用方用 errors.Is 判断；本包不 import httpx（rank 20 < 30），错误分类由边界包做。
var ErrCoverFetch = errors.New("海报下载失败")

// CoverCache 把上游海报落成本地资产，并写 artifact(kind='cover') 索引。
//
// 落盘布局与键见 docs/CACHE-MODEL.md「落盘布局与键」：
// `<cache_dir>/cover/<sha256(key) 前 2 位>/<sha256(key)>.<ext>`，
// key = `<provider>/<provider_id>`。文件内容只由键决定，写用「临时文件 + rename」，
// 因此读到的永远是完整文件。
type CoverCache struct {
	st       *store.Store
	settings *store.Settings
	client   *http.Client
	now      func() time.Time // 取当前时间；测试注入固定值（last_used_at 要可断言）
}

// NewCoverCache 构造海报缓存。client 必须带超时（docs/DEV-STANDARDS.md「错误处理」），
// 由装配根注入。
func NewCoverCache(st *store.Store, settings *store.Settings, client *http.Client) *CoverCache {
	return &CoverCache{st: st, settings: settings, client: client, now: time.Now}
}

// LocalizeCover 确保某部剧的海报在本地：已有索引行且文件在 → 直接返回，不回源。
//
// 这正是「源图失效不影响已入库海报」的实现：一旦落盘，后续刷新不再访问上游。
// 文件缺失或索引行缺失时才重新下载并覆盖索引行。url 为空（上游未声明封面）
// 视为无事可做，返回 nil。
//
// 失败返回 ErrCoverFetch（下载）或 store / 文件系统错误，都不写索引行：宁可在
// 下一轮重试，也不留一条指向不存在文件的 artifact 行。
func (c *CoverCache) LocalizeCover(ctx context.Context, provider, providerID, url string) error {
	if url == "" {
		return nil
	}
	key := provider + "/" + providerID

	// 已在本地：不碰网络。文件在、行在 = 命中；行里的 path 以库为准（可能来自
	// 旧版布局）。
	if row, err := c.st.ArtifactByKey(ctx, store.ArtifactKindCover, key); err == nil {
		if fileExists(row.Path) {
			return nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	body, ext, err := c.fetch(ctx, url)
	if err != nil {
		return err
	}

	dir, err := c.coverDir(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	target := filepath.Join(dir, hash[:2], hash+ext)
	if err := writeFileAtomic(target, body); err != nil {
		return fmt.Errorf("写入海报文件 %s: %w", target, err)
	}

	digest := sha256.Sum256(body)
	if err := c.st.PutArtifact(ctx, store.Artifact{
		Kind:       store.ArtifactKindCover,
		Key:        key,
		Path:       target,
		Bytes:      int64(len(body)),
		SHA256:     hex.EncodeToString(digest[:]),
		LastUsedAt: c.now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	return nil
}

// fetch 取回海报字节并推出文件扩展名（按响应 Content-Type，退化为 URL 后缀）。
func (c *CoverCache) fetch(ctx context.Context, url string) (body []byte, ext string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrCoverFetch, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrCoverFetch, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("%w: 上游状态 %d", ErrCoverFetch, resp.StatusCode)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("%w: 读响应体: %v", ErrCoverFetch, err)
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("%w: 空响应体", ErrCoverFetch)
	}
	return body, coverExt(resp.Header.Get("Content-Type"), url), nil
}

// coverDir 读 cache_dir（每次现读，硬约束 5：改完即时生效）并返回海报目录。
func (c *CoverCache) coverDir(ctx context.Context) (string, error) {
	root, err := c.settings.String(ctx, store.KeyCacheDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cover"), nil
}

// coverExt 由 Content-Type 推扩展名；认不出的 type 退回 URL 后缀，再退回 ".img"。
// 扩展名只影响本地文件名，不影响任何契约。
func coverExt(contentType, url string) string {
	mediatype, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(strings.ToLower(mediatype)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	if i := strings.LastIndexByte(url, '.'); i >= 0 && i > strings.LastIndexByte(url, '/') {
		if suffix := url[i:]; len(suffix) <= 5 {
			return suffix
		}
	}
	return ".img"
}

// writeFileAtomic 在同目录写临时文件再 rename：内容和路径都只由键决定，同名并发
// 写同一份内容无害（docs/CACHE-MODEL.md「并发」）。
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// fileExists 判断路径是否是存在的普通文件。
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
