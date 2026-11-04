package main

// MKACG-683 AC-1 / AC-4：端到端证明「拉合并块的 playlist + 连取分片」会长出
// 一条播放会话，而同步轮次的 /player 请求不产生任何会话。
//
// 用装配根（newAssembly）+ 桩插件 + 真 ffmpeg：分片是真的转封装出来的字节，
// 时长直接写在 episode.duration_sec（每集一片，见 segment.Timeline 的约定）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// playbackSessionView 是 /api/playback 里单条会话的响应形态（只取断言要用的字段）。
type playbackSessionView struct {
	UID         string `json:"uid"`
	DramaID     int64  `json:"drama_id"`
	Title       string `json:"title"`
	Unit        string `json:"unit"`
	Seq         int    `json:"seq"`
	Slices      int    `json:"slices"`
	PositionSec int    `json:"position_sec"`
	TotalSec    int    `json:"total_sec"`
	BytesServed int64  `json:"bytes_served"`
	ClientIP    string `json:"client_ip"`
	UserAgent   string `json:"user_agent"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
}

type playbackView struct {
	Now      string                `json:"now"`
	Streams  int                   `json:"streams"`
	Clients  int                   `json:"clients"`
	Bytes    int64                 `json:"bytes"`
	Sessions []playbackSessionView `json:"sessions"`
}

// TestPlaybackSessionMergedChunk 覆盖 AC-1：合并块 playlist + 3 个分片。
func TestPlaybackSessionMergedChunk(t *testing.T) {
	ctx := context.Background()

	// 假 CDN：一份 6 秒明文，每集都用它（时长以库里的 duration_sec 为准）。
	media, _ := e2ePlainMP4(t, 6)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(media)
	}))
	t.Cleanup(cdn.Close)

	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret:   e2eSecret,
		store.KeyCacheDir:       filepath.Join(t.TempDir(), "cache"),
		store.KeyMergeChunkSize: "3",
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	dramaID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "p1", Title: "合并剧", Year: 2026,
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	for epNo := 1; epNo <= 3; epNo++ {
		// 时长直接给：每集一片，播放位置的期望值由它算出（不依赖 ffprobe 抖动）。
		if _, err := st.UpsertEpisode(ctx, store.Episode{
			DramaID: dramaID, EpNo: epNo, Source: "web", Vid: "v" + strconv.Itoa(epNo),
			DurationSec: 6,
		}); err != nil {
			t.Fatalf("UpsertEpisode %d: %v", epNo, err)
		}
	}

	h := newAssembly(slog.New(slog.DiscardHandler), st, settings, testSyncer(), fakeSchedule{},
		testSources(), stubRegistry(t, "web", cdn.URL+"/direct.mp4"))

	mergedUID := fmt.Sprintf("m%d_1_3", dramaID)
	playlistID := stream.ID(e2eSecret, mergedUID)
	rec := serve(t, h, http.MethodGet, "/video/"+playlistID+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("合并块 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	_, uris := parseE2EPlaylist(t, rec.Body.String())
	if len(uris) != 3 {
		t.Fatalf("合并块分片数 = %d, 期望 3（3 集、每集一片）", len(uris))
	}

	var served int64
	for i, uri := range uris[:3] {
		seg := serve(t, h, http.MethodGet, uri)
		if seg.Code != http.StatusOK {
			t.Fatalf("分片 %d 状态码 = %d, 期望 200（body=%s）", i, seg.Code, seg.Body.String())
		}
		served += int64(seg.Body.Len())
	}

	rec = serve(t, h, http.MethodGet, "/api/playback")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/playback 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	var got playbackView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/api/playback 不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if got.Streams != 1 || got.Clients != 1 {
		t.Fatalf("汇总 = streams:%d clients:%d, 期望 1/1（body=%s）", got.Streams, got.Clients, rec.Body.String())
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("会话数 = %d, 期望 1", len(got.Sessions))
	}
	s := got.Sessions[0]
	if s.UID != mergedUID || s.DramaID != dramaID || s.Title != "合并剧" || s.Unit != "第 1–3 集" {
		t.Errorf("会话剧目字段 = %+v, 期望 uid=%s drama=%d 合并剧/第 1–3 集", s, mergedUID, dramaID)
	}
	if s.Seq != 2 || s.Slices != 3 || s.PositionSec != 12 || s.TotalSec != 18 {
		t.Errorf("会话进度 = seq:%d slices:%d position:%d total:%d, 期望 2/3/12/18",
			s.Seq, s.Slices, s.PositionSec, s.TotalSec)
	}
	if s.BytesServed != served || got.Bytes != served {
		t.Errorf("已出字节 = %d / 汇总 %d, 期望 %d（3 片之和）", s.BytesServed, got.Bytes, served)
	}
	if s.ClientIP == "" || s.FirstSeen == "" || s.LastSeen == "" {
		t.Errorf("客户端或时间字段为空：%+v", s)
	}
}

// fakeSyncFetcher 冒充 syncer：每轮确实打一次上游 /player（同步的真实形态），
// 但它不经过出流路径，因此不该产生播放会话。
type fakeSyncFetcher struct {
	url  string
	hits int
}

func (f *fakeSyncFetcher) Name() string { return "web" }

func (f *fakeSyncFetcher) SyncSeries(ctx context.Context, _ *store.Store, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/player/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		return err
	}
	f.hits++
	return nil
}

// TestPlaybackSyncTrafficNotCounted 覆盖 AC-4：有同步轮次的 /player 请求、
// 没有任何 /video 时，播放列表仍是空数组。
func TestPlaybackSyncTrafficNotCounted(t *testing.T) {
	ctx := context.Background()

	playerHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/player/") {
			playerHits++
		}
		_, _ = io.WriteString(w, "<html><body>详情页</body></html>")
	}))
	t.Cleanup(upstream.Close)

	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret:   e2eSecret,
		store.KeyCacheDir:       filepath.Join(t.TempDir(), "cache"),
		store.KeyMergeChunkSize: "1",
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	fetcher := &fakeSyncFetcher{url: upstream.URL}
	h := newAssembly(slog.New(slog.DiscardHandler), st, settings, fetcher, fakeSchedule{},
		testSources(), stubRegistry(t, "web", "http://127.0.0.1:1/direct.mp4"))

	if err := fetcher.SyncSeries(ctx, st, "sync-session"); err != nil {
		t.Fatalf("SyncSeries: %v", err)
	}
	if playerHits == 0 {
		t.Fatal("同步轮次没有发出 /player 请求，测试没有覆盖到要证明的场景")
	}

	rec := serve(t, h, http.MethodGet, "/api/playback")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/playback 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Errorf("同步后响应体不是空数组：%s", rec.Body.String())
	}
	var got playbackView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/api/playback 不是合法 JSON: %v", err)
	}
	if got.Streams != 0 || got.Clients != 0 || got.Bytes != 0 || len(got.Sessions) != 0 {
		t.Errorf("同步轮次被算成播放：%+v", got)
	}
}
