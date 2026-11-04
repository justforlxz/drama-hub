package vfs

import (
	"context"
	"fmt"

	"github.com/justforlxz/drama-hub/internal/store"
)

// DramaPath 返回一部剧在树里的目录路径（`短剧/<分类目录>/<剧目录>`，不含前导 `/`）。
//
// 管理界面的「WebDAV 路径」用它拼可复制的串，不自己拼目录名：分类目录名（缺失时的
// 兜底名、文件名规则转义）与剧目录名（`<剧名> (<年份>)`，同分类内同名时追加
// ` [<provider>-<provider_id>]` 后缀）只有列举这一处算法（docs/DIRECTORY-CONTRACT.md），
// 第二处拼法必然漂移。
//
// 代价与 listDramaDirs 相同：一次分类存在性点查 + 一次该分类的剧目列举（走
// idx_drama_genre_title），与全库规模无关。
func (t *Tree) DramaPath(ctx context.Context, d store.Drama) (string, error) {
	genreName := genreDir(d.Genre)
	genres, err := t.genresForDir(ctx, genreName)
	if err != nil {
		return "", err
	}
	dirs, err := t.st.ListDramaDirsByGenre(ctx, genres)
	if err != nil {
		return "", fmt.Errorf("列分类剧目: %w", err)
	}
	for _, e := range dramaDirEntries(dirs) {
		if e.drama.ID == d.ID {
			return Root + "/" + genreName + "/" + e.dir, nil
		}
	}
	return "", ErrNotFound
}
