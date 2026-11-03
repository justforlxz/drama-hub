package webdav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/justforlxz/drama-hub/internal/lru"
)

// PROPFIND 响应缓存的容量上限（docs/adr/0015「容量」）。
//
// 实测（8,329 子项的分类目录）一次 PROPFIND 的响应体是 4.5MB，其中序列化占
// 254ms 里的 ~220ms：只缓存 vfs 的结构只能省 30ms，达不到「命中 ≤50ms」的硬门槛，
// 所以缓存必须放在响应层 —— 命中即整块回放字节。
const (
	// propfindCacheMaxBytes 是所有缓存响应体的总和上限。
	propfindCacheMaxBytes = 64 << 20
	// propfindCacheMaxEntry 是单份响应的上限：超过它（10 万项 52.9MB、21.8 万项
	// 114MB 档）不缓存，继续按原路径现算，避免单条把整个缓存挤空。
	propfindCacheMaxEntry = 16 << 20
	// propfindMaxRequestBody 是可缓存的请求体上限。PROPFIND 的体是属性清单，
	// 正常只有几十到几百字节；异常大的体直接不缓存（也不做哈希）。
	propfindMaxRequestBody = 64 << 10
)

// methodPropfind 是 WebDAV 的目录列举方法（net/http 没定义这个常量）。
const methodPropfind = "PROPFIND"

// cachedPropfind 是一份可重放的 PROPFIND 响应。
type cachedPropfind struct {
	header http.Header
	status int
	body   []byte
}

// propfindCache 按「树结构版本号 + 路径 + Depth + 基地址 + 请求体哈希」缓存 207 响应。
//
// 版本号来自 Tree.Revision()：任何影响结构/文件长度的写入都会让它递增，因此命中
// 条件本身就是「数据没变」，不需要按写入路径逐个失效。缓存只在内存里，重启清空。
type propfindCache struct {
	entries  *lru.Cache[cachedPropfind]
	mu       sync.Mutex
	inflight map[string]*propfindGen
}

// propfindGen 是一次进行中的响应生成；同一 key 的并发冷请求等它，不各生成一遍。
type propfindGen struct {
	done chan struct{}
}

func newPropfindCache() *propfindCache {
	return &propfindCache{
		entries:  lru.New[cachedPropfind](propfindCacheMaxBytes, func(e cachedPropfind) int { return len(e.body) }),
		inflight: map[string]*propfindGen{},
	}
}

func (c *propfindCache) get(key string) (cachedPropfind, bool) { return c.entries.Get(key) }

func (c *propfindCache) put(key string, e cachedPropfind) { c.entries.Put(key, e) }

// begin 尝试成为 key 的生成者：返回的 bool 为 false 表示已有别人在生成，调用方
// 应等待 f.done。
func (c *propfindCache) begin(key string) (*propfindGen, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.inflight[key]; ok {
		return f, false
	}
	f := &propfindGen{done: make(chan struct{})}
	c.inflight[key] = f
	return f, true
}

// end 结束一次生成并唤醒等待者。生成者必须先 put 再 end。
func (c *propfindCache) end(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.inflight[key]; ok {
		delete(c.inflight, key)
		close(f.done)
	}
}

// servePropfind 是 PROPFIND 的缓存包装：命中整块回放，未命中转给协议层并尽力
// 把完整响应存起来。认证已在 ServeHTTP 里完成，缓存不会绕过认证。
func (h *Handler) servePropfind(w http.ResponseWriter, r *http.Request) {
	key, cacheable := h.propfindKey(r)
	if cacheable {
		if e, hit := h.propfind.get(key); hit {
			writeCachedPropfind(w, e)
			return
		}
		if f, leader := h.propfind.begin(key); !leader {
			select {
			case <-f.done:
				// 生成者可能没能缓存（响应过大或出错）：那就自己现算一遍。
				if e, hit := h.propfind.get(key); hit {
					writeCachedPropfind(w, e)
					return
				}
			case <-r.Context().Done():
				return
			}
		} else {
			defer h.propfind.end(key)
		}
	}

	rec := &propfindRecorder{ResponseWriter: w, cacheable: cacheable}
	h.inner.ServeHTTP(rec, r)

	if cacheable && rec.status == http.StatusMultiStatus && rec.cacheable {
		h.propfind.put(key, cachedPropfind{header: rec.header, status: rec.status, body: rec.body.Bytes()})
	}
}

// propfindKey 计算缓存键，并把请求体读出来、原样还原（协议层随后要再读一次）。
//
// 键必须包含所有影响响应字节的输入：
//   - 树结构版本号：数据、配置（merge_chunk_size / strm_prefix ……）一变就整体失效；
//   - 路径与 Depth 头：不同目录、不同深度是不同响应；
//   - 对外基地址：.strm 里的绝对地址受它影响，而地址长度出现在 getcontentlength；
//   - 请求体哈希：客户端可以只要一部分属性，属性清单不同响应不同。
//
// 请求体超过上限时返回 false，并且不改动语义（把已读部分与剩余部分拼回去）。
func (h *Handler) propfindKey(r *http.Request) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, propfindMaxRequestBody+1))
	if err != nil {
		return "", false
	}
	if len(body) > propfindMaxRequestBody {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s",
		h.tree.Revision(), r.URL.Path, r.Header.Get("Depth"),
		requestBaseURL(r), hex.EncodeToString(sum[:])), true
}

// writeCachedPropfind 回放一份缓存响应。Content-Length 由缓存补齐（协议层是流式
// 写出的，只有在这里才知道完整长度）。
func writeCachedPropfind(w http.ResponseWriter, e cachedPropfind) {
	for k, vs := range e.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Length") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(e.body)))
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

// propfindRecorder 是「边写边存」的 ResponseWriter 包装：把 207 响应体复制一份，
// 超过单条上限就放弃缓存继续透传。
type propfindRecorder struct {
	http.ResponseWriter
	status    int
	header    http.Header
	body      bytes.Buffer
	cacheable bool
	wrote     bool
}

// WriteHeader 记录首个状态码与当时的响应头。
//
// 第二个状态码意味着协议层在正文写出后才发现错误（例如某个子项算不出来，
// handlePropfind 返回 500）：已经发出去的是不完整响应，不能缓存。
func (rec *propfindRecorder) WriteHeader(code int) {
	if rec.wrote {
		rec.cacheable = false
		return
	}
	rec.wrote = true
	rec.status = code
	rec.header = rec.ResponseWriter.Header().Clone()
	rec.ResponseWriter.WriteHeader(code)
}

// Write 透传字节并复制一份；底层写失败或短写时不缓存。
func (rec *propfindRecorder) Write(p []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(p)
	if err != nil || n != len(p) {
		rec.cacheable = false
	}
	if !rec.cacheable {
		return n, err
	}
	if rec.body.Len()+len(p) > propfindCacheMaxEntry {
		rec.cacheable = false
		rec.body.Reset()
		return n, err
	}
	rec.body.Write(p)
	return n, err
}
