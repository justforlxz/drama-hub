package web

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// pagingRunner 是 paging.js 的表驱动断言。它 require 被测的同一份源码 ——
// 浏览器与测试跑的是同一个函数，不重写第二份分页逻辑。
//
// 最后一行输出台账列名，供 Go 侧与 library.html 的表头逐字对照。
const pagingRunner = `'use strict';
const assert = require('assert');
const P = require(process.argv[2]);

function item(i, extra) {
  return Object.assign({
    id: i, title: '剧 ' + i, provider: 'web', provider_id: String(100000 + i),
    genre: '都市', year: 2024, episodes: 3, episode_total: 5, has_cover: i % 2 === 0,
    emby_items: 1, sync_at: '2026-10-30T08:00:00Z',
  }, extra || {});
}

// 每页 50 条
assert.strictEqual(P.PAGE_SIZE, 50);

const many = Array.from({ length: 1400 }, (_, i) => item(i + 1));

// 第 1 页 50 条、28 页
const first = P.computeView(many, { page: 1 });
assert.strictEqual(first.items.length, 50);
assert.strictEqual(first.pages, 28);
assert.strictEqual(first.page, 1);
assert.strictEqual(first.total, 1400);
assert.strictEqual(first.items[0].id, 1);

// 越界页夹到末页，且不越界
const last = P.computeView(many, { page: 999 });
assert.strictEqual(last.page, 28);
assert.strictEqual(last.items.length, 50);
assert.strictEqual(last.items[last.items.length - 1].id, 1400);

// 空列表也是 1 页 0 条
const empty = P.computeView([], { page: 3 });
assert.strictEqual(empty.pages, 1);
assert.strictEqual(empty.page, 1);
assert.strictEqual(empty.items.length, 0);

// 搜索匹配剧名与上游 id，空串等于不筛
const list = [
  item(1, { title: '隐秘的角落' }),
  item(2, { title: '漫长的季节' }),
  item(3, { title: '另一个', provider_id: '555' }),
];
assert.deepStrictEqual(P.filterDramas(list, { query: '隐秘' }).map((d) => d.id), [1]);
assert.deepStrictEqual(P.filterDramas(list, { query: '555' }).map((d) => d.id), [3]);
assert.strictEqual(P.filterDramas(list, { query: '  ' }).length, 3);

// 分类筛选；空分类归「其他」
const genres = [item(1, { genre: '都市' }), item(2, { genre: '悬疑' }), item(3, { genre: '' })];
assert.deepStrictEqual(P.filterDramas(genres, { genre: '悬疑' }).map((d) => d.id), [2]);
assert.deepStrictEqual(P.filterDramas(genres, { genre: '其他' }).map((d) => d.id), [3]);
assert.strictEqual(P.genreOf({ genre: '' }), '其他');

// 状态筛选
const statuses = [
  item(1, { has_cover: false, episodes: 2, episode_total: 5 }),
  item(2, { has_cover: true, episodes: 5, episode_total: 5 }),
];
assert.deepStrictEqual(P.filterDramas(statuses, { status: 'no-cover' }).map((d) => d.id), [1]);
assert.deepStrictEqual(P.filterDramas(statuses, { status: 'incomplete' }).map((d) => d.id), [1]);
assert.strictEqual(P.filterDramas(statuses, { status: '' }).length, 2);

// 改筛选条件回到第 1 页，其余状态不动
const moved = P.withFilter({ query: '', genre: '', status: '', page: 7, view: 'wall' }, { genre: '都市' });
assert.strictEqual(moved.page, 1);
assert.strictEqual(moved.genre, '都市');
assert.strictEqual(moved.view, 'wall');

// 汇总
assert.deepStrictEqual(P.totals([item(1), item(2)]), { dramas: 2, episodes: 6, emby_items: 2 });

// 台账单元格：9 列、label 与列名一致、集数是 episodes/episode_total
const cells = P.ledgerCells(item(1, { has_cover: true }), () => '2026-10-30 16:00', () => '已入库');
assert.strictEqual(cells.length, P.LEDGER_COLUMNS.length);
assert.deepStrictEqual(cells.map((c) => c.label), P.LEDGER_COLUMNS);
assert.strictEqual(cells[0].text, '有封面');
assert.strictEqual(cells[4].text, '3/5');
assert.strictEqual(cells[5].text, 'web:100001');
assert.strictEqual(P.ledgerCells(item(2, { has_cover: false }), () => '—', () => '—')[0].text, '无封面');

// 块的集号区间标签
assert.strictEqual(P.chunkRangeLabel(3, 3), '第 3 集');
assert.strictEqual(P.chunkRangeLabel(3, 12), '第 3-12 集');

console.log(JSON.stringify(P.LEDGER_COLUMNS));
`

// TestPagingContract 用 node 跑 paging.js 的表驱动单测（AC-8：分页与筛选是
// 纯函数；容器内 e2e 镜像装了 nodejs，node 只用于这里，不进运行时镜像）。
// 最后把 node 报出来的列名与 /library 表头对照 —— 分页逻辑与模板是同一契约的两端。
func TestPagingContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("跑 paging.js 单测需要 node（e2e 测试镜像装 nodejs 就是为了这个）：%v", err)
	}
	source, err := assets.ReadFile("static/paging.js")
	if err != nil {
		t.Fatalf("读 paging.js: %v", err)
	}

	dir := t.TempDir()
	pagingPath := filepath.Join(dir, "paging.js")
	if err := os.WriteFile(pagingPath, source, 0o644); err != nil {
		t.Fatalf("写 paging.js: %v", err)
	}
	runnerPath := filepath.Join(dir, "runner.js")
	if err := os.WriteFile(runnerPath, []byte(pagingRunner), 0o644); err != nil {
		t.Fatalf("写 runner.js: %v", err)
	}

	out, err := exec.Command(node, runnerPath, pagingPath).CombinedOutput()
	if err != nil {
		t.Fatalf("paging.js 断言失败: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var columns []string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &columns); err != nil {
		t.Fatalf("解析列名输出 %q: %v", lines[len(lines)-1], err)
	}

	srv, _, _ := newTestServer(t)
	doc := parseDoc(t, do(t, srv, http.MethodGet, "/library", "").Body.Bytes())
	ledger := byID(doc, "library-ledger")
	if ledger == nil {
		t.Fatal("/library 没有台账，#library-ledger 缺失")
	}
	var headers []string
	eachNode(ledger, func(n *html.Node) {
		if attrOf(n, "role") == "columnheader" {
			headers = append(headers, strings.TrimSpace(legendText(n)))
		}
	})
	if strings.Join(headers, ",") != strings.Join(columns, ",") {
		t.Errorf("台账表头 = %v，paging.js 的 LEDGER_COLUMNS = %v", headers, columns)
	}
}
