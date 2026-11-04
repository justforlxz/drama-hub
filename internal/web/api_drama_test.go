package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// TestAPI_ListDramasGenreAndCover 覆盖列表页新增的两个字段（AC-3）：
// genre 直出、has_cover 由 artifact 索引派生（没有海报的剧返回 false 而不是报错）。
func TestAPI_ListDramasGenreAndCover(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	withGenre, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "7001", Title: "有分类的剧", Year: 2024,
		Genre: "都市", EpisodeTotal: 2, SyncAt: "2026-10-30T08:00:00Z",
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	withoutCover := seedDrama(t, st, "7002", "没分类没封面的剧", []string{"g1"})

	list := func() map[string]dramaView {
		t.Helper()
		rec := do(t, srv, http.MethodGet, "/api/dramas", "")
		got := decode[dramaList](t, rec)
		byProvider := make(map[string]dramaView, len(got.Dramas))
		for _, d := range got.Dramas {
			byProvider[d.ProviderID] = d
		}
		return byProvider
	}

	dramas := list()
	if d := dramas["7001"]; d.ID != withGenre || d.Genre != "都市" || d.HasCover {
		t.Errorf("7001 的视图 = id:%d genre:%q has_cover:%v, 期望 id:%d genre:都市 has_cover:false", d.ID, d.Genre, d.HasCover, withGenre)
	}
	if d := dramas["7002"]; d.ID != withoutCover || d.Genre != "" || d.HasCover {
		t.Errorf("7002 的视图 = id:%d genre:%q has_cover:%v, 期望 id:%d genre:\"\" has_cover:false", d.ID, d.Genre, d.HasCover, withoutCover)
	}

	// 缓存里出现该剧的海报索引行后，has_cover 立刻变 true；另一部不受影响。
	if err := st.PutArtifact(ctx, store.Artifact{
		Kind: store.ArtifactKindCover, Key: "web/7001", Path: "covers/web/7001.bin",
		Bytes: 8, LastUsedAt: "2026-10-30T08:00:00Z",
	}); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	dramas = list()
	if d := dramas["7001"]; !d.HasCover {
		t.Error("7001 已有海报索引行，has_cover 仍是 false")
	}
	if d := dramas["7002"]; d.HasCover {
		t.Error("7002 没有海报索引行，has_cover 却是 true")
	}
}

// TestAPI_DramaEpisodes 覆盖剧详情页的集分块接口（AC-4）：块边界与对外集号
// 必须与 store 的派生逐条一致，改 merge_chunk_size 后重取随之变化；
// 播放 id 只由注入的签名函数下发。
func TestAPI_DramaEpisodes(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	// stream_secret 是必填项（生产里由装配根在首次启动时生成），剧集端点要用它签名。
	if err := srv.settings.Set(ctx, store.KeyStreamSecret, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("设流出签名密钥: %v", err)
	}
	id := seedDrama(t, st, "7101", "五集的剧", []string{"a", "b", "c", "d", "e"})

	// 与 store 派生逐条对照：块是排序集列表的连续切片。
	compare := func(view dramaEpisodesView, n int) {
		t.Helper()
		if view.ID != id {
			t.Errorf("id = %d, 期望 %d", view.ID, id)
		}
		if view.MergeChunkSize != n {
			t.Errorf("merge_chunk_size = %d, 期望 %d", view.MergeChunkSize, n)
		}
		items, err := st.ListEmbyItems(ctx, id)
		if err != nil {
			t.Fatalf("ListEmbyItems: %v", err)
		}
		if len(view.Chunks) != len(items) {
			t.Fatalf("块数 = %d, store 派生 %d", len(view.Chunks), len(items))
		}
		secret, err := srv.settings.String(ctx, store.KeyStreamSecret)
		if err != nil {
			t.Fatalf("读流出签名密钥: %v", err)
		}
		for i, item := range items {
			chunk := view.Chunks[i]
			if chunk.EpNo != item.EpNo || chunk.From != item.EpFrom || chunk.To != item.EpTo {
				t.Errorf("第 %d 块 = ep_no:%d %d-%d, 期望 ep_no:%d %d-%d",
					i, chunk.EpNo, chunk.From, chunk.To, item.EpNo, item.EpFrom, item.EpTo)
			}
			if chunk.Count != item.EpTo-item.EpFrom+1 {
				t.Errorf("第 %d 块 count = %d, 期望 %d", i, chunk.Count, item.EpTo-item.EpFrom+1)
			}
			if want := testSignID(secret, item.UID); chunk.PlayID != want {
				t.Errorf("第 %d 块 play_id = %q, 期望 %q", i, chunk.PlayID, want)
			}
			if !strings.Contains(chunk.PlayURL, "/video/") || !strings.HasSuffix(chunk.PlayURL, ".m3u8") {
				t.Errorf("第 %d 块 play_url = %q, 期望含 /video/ 且以 .m3u8 结尾", i, chunk.PlayURL)
			}
			if !strings.Contains(chunk.PlayURL, chunk.PlayID) {
				t.Errorf("第 %d 块 play_url = %q 里没有 play_id %q", i, chunk.PlayURL, chunk.PlayID)
			}
		}
	}

	path := "/api/dramas/" + strconv.FormatInt(id, 10) + "/episodes"

	if err := srv.settings.Set(ctx, store.KeyMergeChunkSize, "2"); err != nil {
		t.Fatalf("设 merge_chunk_size=2: %v", err)
	}
	view := decode[dramaEpisodesView](t, do(t, srv, http.MethodGet, path, ""))
	compare(view, 2)
	if len(view.Chunks) != 3 {
		t.Fatalf("5 集按 2 合块应有 3 块, 实际 %d", len(view.Chunks))
	}
	wantRanges := [][2]int{{1, 2}, {3, 4}, {5, 5}}
	wantCounts := []int{2, 2, 1}
	for i, chunk := range view.Chunks {
		if chunk.From != wantRanges[i][0] || chunk.To != wantRanges[i][1] || chunk.Count != wantCounts[i] {
			t.Errorf("第 %d 块 = %d-%d/%d, 期望 %d-%d/%d",
				i, chunk.From, chunk.To, chunk.Count, wantRanges[i][0], wantRanges[i][1], wantCounts[i])
		}
		if len(chunk.Episodes) != wantCounts[i] {
			t.Errorf("第 %d 块带了 %d 集, 期望 %d", i, len(chunk.Episodes), wantCounts[i])
		}
	}
	if view.WebDAVPath == "" {
		t.Error("webdav_path 为空：详情页无法给出树形路径")
	}

	// 改成分 3 结合并 → 2 块（1-3、4-5）。
	if err := srv.settings.Set(ctx, store.KeyMergeChunkSize, "3"); err != nil {
		t.Fatalf("设 merge_chunk_size=3: %v", err)
	}
	view = decode[dramaEpisodesView](t, do(t, srv, http.MethodGet, path, ""))
	compare(view, 3)
	if len(view.Chunks) != 2 || view.Chunks[0].From != 1 || view.Chunks[0].To != 3 || view.Chunks[1].From != 4 || view.Chunks[1].To != 5 {
		t.Errorf("按 3 合块的区间 = %+v, 期望 1-3、4-5", view.Chunks)
	}

	// id 非法 400、不存在 404。
	if rec := do(t, srv, http.MethodGet, "/api/dramas/abc/episodes", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("GET /api/dramas/abc/episodes 状态码 = %d, 期望 400", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/dramas/999999/episodes", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/dramas/999999/episodes 状态码 = %d, 期望 404", rec.Code)
	}
}

// TestAPI_DramaCover 覆盖海报字节透传（AC-5）：命中 200 + image/* + 精确长度；
// 没有海报与上游失败都是 404（不是 500），剧不存在也是 404。
func TestAPI_DramaCover(t *testing.T) {
	srv, st, _ := newTestServer(t)
	id := seedDrama(t, st, "7201", "有封面的剧", []string{"p1"})
	path := "/api/dramas/" + strconv.FormatInt(id, 10) + "/cover"

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
	srv.covers = &fakeCovers{body: png, available: true}
	rec := do(t, srv, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cover 状态码 = %d, 期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("Content-Type = %q, 期望 image/*", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
	}
	if rec.Body.Len() != len(png) {
		t.Errorf("海报字节数 = %d, 期望 %d", rec.Body.Len(), len(png))
	}
	if cc := rec.Header().Get("Cache-Control"); cc == "" {
		t.Error("海报响应缺少 Cache-Control")
	}

	// 没有海报 / 上游失败：cache.Poster 统一返回 available=false → 404。
	miss := &fakeCovers{available: false}
	srv.covers = miss
	if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("无海报状态码 = %d, 期望 404", rec.Code)
	}
	if miss.calls != 1 {
		t.Errorf("无海报时调用缓存 %d 次, 期望 1 次（由缓存层判定是否存在）", miss.calls)
	}

	// 剧不存在：不查缓存直接 404。
	srv.covers = &fakeCovers{available: true}
	if rec := do(t, srv, http.MethodGet, "/api/dramas/999999/cover", ""); rec.Code != http.StatusNotFound {
		t.Errorf("剧不存在的状态码 = %d, 期望 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/dramas/abc/cover", ""); rec.Code != http.StatusNotFound {
		t.Errorf("id 非法的状态码 = %d, 期望 404", rec.Code)
	}
	if srv.covers.(*fakeCovers).calls != 0 {
		t.Errorf("剧不存在/id 非法时不该查缓存, 实际调用 %d 次", srv.covers.(*fakeCovers).calls)
	}
}
