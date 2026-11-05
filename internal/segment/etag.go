package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// playlistETag 是播放列表正文的内容哈希（强 ETag，MKACG-682）。
//
// BuildPlaylist 是纯函数：同一播放单元、同一批时长 → 逐字节相同的正文 → 同一个
// ETag；改 merge_chunk_size、时长回填或成员变化都会改变正文，ETag 随之改变。
func playlistETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches 判定 If-None-Match 是否命中。支持 `*` 与逗号分隔的多个标签；
// 弱标签（`W/"…"`）按弱比较处理，与强标签等价 —— m3u8 是纯文本派生视图，
// 强/弱没有实际差别。
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
