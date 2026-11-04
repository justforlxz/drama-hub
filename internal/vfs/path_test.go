package vfs

import (
	"context"
	"errors"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// TestTreeDramaPath 锁 Tree.DramaPath（详情页「WebDAV 路径」的唯一来源）：
// 路径必须与树里真实列出的目录逐字一致 —— 分类目录的兜底名、剧目录的
// ` [<provider>-<provider_id>]` 冲突后缀，任何一处漂了，复制出来的路径就打不开。
func TestTreeDramaPath(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	st := f.store()

	dramaA, err := st.DramaByProviderID(ctx, "web", "a1")
	if err != nil {
		t.Fatalf("DramaByProviderID a1: %v", err)
	}

	// 1. 命中：分类目录 + 剧目录，与 List 出来的路径同形。
	got, err := f.tree.DramaPath(ctx, dramaA)
	if err != nil {
		t.Fatalf("DramaPath 剧 A: %v", err)
	}
	if want := f.dirA; got != want {
		t.Errorf("剧 A 的路径 = %q, 期望 %q", got, want)
	}

	// 2. 没填分类的剧落兜底目录「其他」。
	dramaB, err := st.DramaByProviderID(ctx, "app", "b1")
	if err != nil {
		t.Fatalf("DramaByProviderID b1: %v", err)
	}
	got, err = f.tree.DramaPath(ctx, dramaB)
	if err != nil {
		t.Fatalf("DramaPath 剧 B: %v", err)
	}
	if want := f.dirB; got != want {
		t.Errorf("剧 B 的路径 = %q, 期望 %q", got, want)
	}

	// 3. 同分类、同名、同年：冲突成员都改写成带 provider-id 后缀的目录名
	//    （先加的那部也一起改 —— 目录唯一性优先于「谁先来」）。
	cID := f.addDrama(t, store.Drama{
		Provider: "web", ProviderID: "a2", Title: titleA, Year: 2024, Genre: genreA,
	})
	dramaC, err := st.DramaByID(ctx, cID)
	if err != nil {
		t.Fatalf("DramaByID %d: %v", cID, err)
	}
	wantA := Root + "/" + genreA + "/" + titleA + " (2024) [web-a1]"
	wantC := Root + "/" + genreA + "/" + titleA + " (2024) [web-a2]"
	gotA, err := f.tree.DramaPath(ctx, dramaA)
	if err != nil {
		t.Fatalf("DramaPath 冲突下的剧 A: %v", err)
	}
	gotC, err := f.tree.DramaPath(ctx, dramaC)
	if err != nil {
		t.Fatalf("DramaPath 剧 C: %v", err)
	}
	if gotA != wantA || gotC != wantC {
		t.Errorf("冲突目录 = %q / %q, 期望 %q / %q", gotA, gotC, wantA, wantC)
	}
	// 这两个目录名必须真的出现在树里（而不是只由本函数自己拼出来）。
	nodes, err := f.tree.List(ctx, Root+"/"+genreA)
	if err != nil {
		t.Fatalf("List %s: %v", Root+"/"+genreA, err)
	}
	listed := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		listed[n.Path] = true
	}
	for _, path := range []string{wantA, wantC} {
		if !listed[path] {
			t.Errorf("树里没有目录 %q，DramaPath 与列举算法漂了：实际 %v", path, nodes)
		}
	}

	// 4. 分类存在但这部剧不在里面 → ErrNotFound（详情页只对存在的剧渲染）。
	ghost := dramaA
	ghost.ID = 999999
	if _, err := f.tree.DramaPath(ctx, ghost); !errors.Is(err, ErrNotFound) {
		t.Errorf("不在该分类的剧返回 %v, 期望 ErrNotFound", err)
	}
}
