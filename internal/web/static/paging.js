'use strict';
/* 剧库页的纯逻辑：筛选、分页、台账单元格。
 * 不碰 DOM，浏览器里挂在 window.DramaHubPaging，node 里可 require（见 paging_test.go 的表驱动单测）。 */

(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.DramaHubPaging = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  // 一页 50 条；后端仍是全量 ListDramas（已知边界，见 ADR-0016）。
  const PAGE_SIZE = 50;

  // 台账的列顺序，和 library.html 里的 role=columnheader 一一对应。
  const LEDGER_COLUMNS = ['封面', '标题', '年份', '分类', '集数', '上游 id', '上次同步', 'Jellyfin', '操作'];

  /** 把页码夹到 [1, 总页数]；空列表也有 1 页。 */
  function clampPage(page, total, size) {
    const per = size || PAGE_SIZE;
    const pages = Math.max(1, Math.ceil(total / per));
    const value = Number.isFinite(page) ? Math.trunc(page) : 1;
    if (value < 1) return 1;
    if (value > pages) return pages;
    return value;
  }

  /** 分页切片：返回夹紧后的页码、总页数与本页条目。 */
  function pageSlice(items, page, size) {
    const per = size || PAGE_SIZE;
    const total = items.length;
    const current = clampPage(page, total, per);
    const start = (current - 1) * per;
    return {
      page: current,
      pages: Math.max(1, Math.ceil(total / per)),
      total,
      items: items.slice(start, start + per),
    };
  }

  /** 分类没填的剧归到「其他」（与树形目录一致）。 */
  function genreOf(drama) {
    return drama.genre && drama.genre !== '' ? drama.genre : '其他';
  }

  /** 搜索框匹配剧名或上游 id；状态筛选分「无封面」「集数不齐」。 */
  function matches(drama, filter) {
    const query = (filter.query || '').trim().toLowerCase();
    if (query !== '') {
      const haystack = (drama.title || '') + ' ' + (drama.provider_id || '');
      if (!haystack.toLowerCase().includes(query)) return false;
    }
    if (filter.genre && genreOf(drama) !== filter.genre) return false;
    if (filter.status === 'no-cover' && drama.has_cover) return false;
    if (filter.status === 'incomplete' && drama.episodes >= drama.episode_total) return false;
    return true;
  }

  /** 纯筛选：不改变入参，顺序保持后端给的（按 id）。 */
  function filterDramas(list, filter) {
    const f = filter || {};
    return list.filter((drama) => matches(drama, f));
  }

  /** 筛选 + 分页一次算完；page 越界时夹回边界。 */
  function computeView(list, filter) {
    const f = filter || {};
    const view = pageSlice(filterDramas(list, f), f.page);
    view.genre = f.genre || '';
    view.status = f.status || '';
    view.query = f.query || '';
    return view;
  }

  /** 改了任一筛选条件就回到第 1 页。 */
  function withFilter(state, patch) {
    return Object.assign({}, state, patch, { page: 1 });
  }

  /** 剧库汇总一句话里的三段数字。 */
  function totals(list) {
    let episodes = 0;
    let embyItems = 0;
    for (const drama of list) {
      episodes += drama.episodes || 0;
      embyItems += drama.emby_items || 0;
    }
    return { dramas: list.length, episodes, emby_items: embyItems };
  }

  /** 台账一行的 9 个单元格文本；封面/标题/操作由页面换成图片或链接。 */
  function ledgerCells(drama, fmtTime, jellyfinText) {
    const time = typeof fmtTime === 'function' ? fmtTime : (value) => value || '—';
    const jf = typeof jellyfinText === 'function' ? jellyfinText : () => '—';
    const values = [
      drama.has_cover ? '有封面' : '无封面',
      drama.title || '—',
      drama.year ? String(drama.year) : '—',
      genreOf(drama),
      (drama.episodes || 0) + '/' + (drama.episode_total || 0),
      (drama.provider || '') + ':' + (drama.provider_id || ''),
      time(drama.sync_at),
      jf(genreOf(drama)),
      '详情',
    ];
    return LEDGER_COLUMNS.map((label, i) => ({ label, text: values[i] }));
  }

  /** 「第 3-12 集」/「第 3 集」。 */
  function chunkRangeLabel(from, to) {
    return from === to ? '第 ' + from + ' 集' : '第 ' + from + '-' + to + ' 集';
  }

  return {
    PAGE_SIZE,
    LEDGER_COLUMNS,
    clampPage,
    pageSlice,
    genreOf,
    filterDramas,
    computeView,
    withFilter,
    totals,
    ledgerCells,
    chunkRangeLabel,
  };
}));
