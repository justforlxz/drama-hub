'use strict';
// 管理页脚本：只用原生 DOM，不引入框架。所有数据来自 /api/ JSON，页面模板只给骨架。
// 写操作统一带 X-Admin-Token；令牌只存在当前标签页的 sessionStorage。

const TOKEN_KEY = 'drama-hub.admin-token';
const TOKEN_HEADER = 'X-Admin-Token';
// 令牌变化（输入、解锁、清除、关闭弹窗）统一广播：各页据此重算写操作可用性，
// 避免控件只在首次渲染时算一次 disabled，解锁后还得刷新页面。
const TOKEN_EVENT = 'admin-token-changed';

/* ---------- 管理令牌 ---------- */

function adminToken() {
  const input = document.getElementById('admin-token');
  if (input && input.value.trim() !== '') return input.value.trim();
  return sessionStorage.getItem(TOKEN_KEY) || '';
}

function initAdminToken() {
  const input = document.getElementById('admin-token');
  if (!input) return;
  input.value = sessionStorage.getItem(TOKEN_KEY) || '';
  input.addEventListener('change', () => {
    const value = input.value.trim();
    if (value === '') sessionStorage.removeItem(TOKEN_KEY);
    else sessionStorage.setItem(TOKEN_KEY, value);
  });
}

function updateTokenState() {
  const state = document.getElementById('token-state');
  const button = document.getElementById('token-button');
  const unlocked = adminToken() !== '';
  if (state) state.textContent = unlocked ? '写操作 · 已解锁' : '写操作 · 未解锁';
  if (button) button.dataset.unlocked = unlocked ? 'true' : 'false';
  document.dispatchEvent(new CustomEvent(TOKEN_EVENT));
}

function openTokenDialog() {
  const dialog = document.getElementById('token-dialog');
  if (!dialog) return;
  if (typeof dialog.showModal === 'function') dialog.showModal();
  else dialog.setAttribute('open', '');
  const input = document.getElementById('admin-token');
  if (input) input.focus();
}

function closeTokenDialog() {
  const dialog = document.getElementById('token-dialog');
  if (!dialog) return;
  if (typeof dialog.close === 'function') dialog.close();
  else dialog.removeAttribute('open');
}

function initTokenDialog() {
  const button = document.getElementById('token-button');
  const dialog = document.getElementById('token-dialog');
  const form = document.getElementById('token-form');
  const clear = document.getElementById('token-clear');
  const input = document.getElementById('admin-token');
  const notice = document.getElementById('token-notice');

  if (button) button.addEventListener('click', openTokenDialog);
  if (clear) {
    clear.addEventListener('click', () => {
      if (input) input.value = '';
      sessionStorage.removeItem(TOKEN_KEY);
      if (notice) notice.textContent = '';
      updateTokenState();
      closeTokenDialog();
      say('已清除管理令牌，写操作要重新解锁', 'ok');
    });
  }
  if (form) {
    form.addEventListener('submit', () => {
      if (input) {
        const value = input.value.trim();
        if (value === '') sessionStorage.removeItem(TOKEN_KEY);
        else sessionStorage.setItem(TOKEN_KEY, value);
      }
      updateTokenState();
    });
  }
  if (dialog) {
    dialog.addEventListener('close', () => {
      if (notice) notice.textContent = '';
      updateTokenState();
    });
  }
  if (input) input.addEventListener('change', updateTokenState);
  updateTokenState();
}

/* ---------- 请求与 DOM 工具 ---------- */

// apiRaw 返回 {status, data}；非 2xx 抛错（带 status），401 顺势打开令牌弹窗。
async function apiRaw(method, path, body) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET') headers[TOKEN_HEADER] = adminToken();
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data = null;
  if (text !== '') {
    try {
      data = JSON.parse(text);
    } catch (err) {
      data = null;
    }
  }
  if (!res.ok) {
    const message = data && data.message ? data.message : res.status + ' ' + res.statusText;
    const error = new Error(message);
    error.status = res.status;
    if (res.status === 401) {
      openTokenDialog();
      const notice = document.getElementById('token-notice');
      if (notice) notice.textContent = message;
    }
    throw error;
  }
  return { status: res.status, data };
}

async function api(method, path, body) {
  return (await apiRaw(method, path, body)).data;
}

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === undefined || value === null) continue;
    if (key === 'text') node.textContent = value;
    else if (key === 'class') node.className = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value);
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function say(text, kind, id) {
  const box = document.getElementById(id || 'notice');
  if (!box) return;
  box.textContent = text;
  box.dataset.kind = kind || '';
}

function fail(err) {
  say(err && err.message ? err.message : String(err), 'error');
}

function fmtTime(iso) {
  if (!iso) return '—';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  const pad = (n) => String(n).padStart(2, '0');
  return (
    date.getFullYear() +
    '-' +
    pad(date.getMonth() + 1) +
    '-' +
    pad(date.getDate()) +
    ' ' +
    pad(date.getHours()) +
    ':' +
    pad(date.getMinutes())
  );
}

function fmtDuration(ms) {
  if (typeof ms !== 'number' || ms < 0 || Number.isNaN(ms)) return '—';
  if (ms < 1000) return '不到 1 秒';
  const sec = ms / 1000;
  if (sec < 60) return sec.toFixed(1) + ' 秒';
  const min = Math.floor(sec / 60);
  const rest = Math.round(sec % 60);
  return min + ' 分 ' + rest + ' 秒';
}

function fmtBytes(n) {
  if (typeof n !== 'number' || n < 0 || Number.isNaN(n)) return '—';
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = n / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return value.toFixed(value >= 10 ? 0 : 1) + ' ' + units[unit];
}

function fmtClock(sec) {
  if (typeof sec !== 'number' || sec < 0 || Number.isNaN(sec)) return '—';
  const total = Math.round(sec);
  const m = Math.floor(total / 60);
  const s = total % 60;
  return m + ':' + String(s).padStart(2, '0');
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// genreDir 与 internal/vfs/tree.go 的分类目录名保持一致：空分类落到「其他」，
// 非法字符逐个替换成下划线。
function genreDir(genre) {
  return (genre === '' || genre === undefined || genre === null ? '其他' : genre).replace(/[/\\:*?"<>|]/g, '_');
}

/* ---------- 概览 ---------- */

function initOverview() {
  const systemBadge = document.getElementById('system-badge');
  const systemTitle = document.getElementById('system-title');
  const systemCopy = document.getElementById('system-copy');
  const systemNote = document.getElementById('system-note');
  const syncState = document.getElementById('sync-state');
  const sourceState = document.getElementById('source-state');
  const dramasEl = document.getElementById('summary-dramas');
  const episodesEl = document.getElementById('summary-episodes');
  const embyEl = document.getElementById('summary-emby');
  const summaryEl = document.getElementById('summary');
  const syncBadge = document.getElementById('sync-badge');
  const lastRunEl = document.getElementById('sync-last-run');
  const lastOkEl = document.getElementById('sync-last-ok');
  const nextEl = document.getElementById('sync-next');
  const pbStreams = document.getElementById('playback-streams');
  const pbSummary = document.getElementById('playback-summary');
  const pbBadge = document.getElementById('playback-badge');
  const pbEmpty = document.getElementById('playback-empty');
  const pbList = document.getElementById('playback-list');

  const PLAYBACK_POLL_MS = 4000;
  let playbackRunning = false;

  function latest(list, key) {
    let best = '';
    for (const item of list) {
      const value = item[key] || '';
      if (value > best) best = value;
    }
    return best;
  }

  function earliest(list, key) {
    let best = '';
    for (const item of list) {
      const value = item[key] || '';
      if (value !== '' && (best === '' || value < best)) best = value;
    }
    return best;
  }

  function setIcon(tone, glyph) {
    if (!systemBadge) return;
    systemBadge.textContent = glyph;
    if (tone) systemBadge.dataset.tone = tone;
    else delete systemBadge.dataset.tone;
  }

  function renderSystem(st) {
    const sources = st.sources || [];
    const jf = st.jellyfin || {};
    const running = sources.some((s) => s.running) || jf.running === true;
    const failing = sources.filter((s) => s.last_error);
    const disabled = sources.filter((s) => !s.enabled);
    const notes = [];

    if (!jf.configured) notes.push('Jellyfin 连接未配置');
    if (failing.length > 0) notes.push(failing.length + ' 个播放源上次失败');
    if (disabled.length > 0) notes.push(disabled.length + ' 个播放源已停用');

    if (running) {
      setIcon('warn', '↻');
      systemTitle.textContent = '正在同步…';
      systemCopy.textContent = '播放源或 Jellyfin 正在运行，完成后这里会自动更新。';
    } else if (failing.length > 0) {
      setIcon('danger', '!');
      systemTitle.textContent = '运行中，有播放源异常';
      systemCopy.textContent = notes.join('；') + '。';
    } else if (notes.length > 0) {
      setIcon('warn', '!');
      systemTitle.textContent = '运行正常，有未完成配置';
      systemCopy.textContent = notes.join('；') + '。';
    } else {
      setIcon('', '✓');
      systemTitle.textContent = '运行正常';
      systemCopy.textContent = '所有播放源都在按计划扫描，托管媒体库保持同步。';
    }

    const now = new Date();
    systemNote.textContent = '读取于 ' + String(now.getHours()).padStart(2, '0') + ':' + String(now.getMinutes()).padStart(2, '0');

    const okAt = latest(sources, 'last_ok_at');
    syncState.textContent = okAt === '' ? '还没有成功过' : '最近成功 ' + fmtTime(okAt);
    if (running) sourceState.textContent = '正在运行';
    else if (sources.length === 0) sourceState.textContent = '没有启用的播放源';
    else if (failing.length > 0) sourceState.textContent = sources.length + ' 个播放源 · ' + failing.length + ' 个异常';
    else sourceState.textContent = sources.length + ' 个播放源运行中';

    if (syncBadge) {
      syncBadge.textContent = running ? '同步中' : failing.length > 0 ? '有异常' : '正常';
      syncBadge.dataset.tone = running ? 'warn' : failing.length > 0 ? 'danger' : 'ok';
    }
  }

  function renderSummary(st) {
    const totals = st.totals || {};
    dramasEl.textContent = String(totals.dramas || 0);
    episodesEl.textContent = String(totals.episodes || 0);
    embyEl.textContent = String(totals.emby_items || 0);
    summaryEl.textContent = '最近同步 ' + fmtTime(totals.last_sync_at);

    const sources = st.sources || [];
    lastRunEl.textContent = fmtTime(latest(sources, 'last_run_at'));
    lastOkEl.textContent = fmtTime(latest(sources, 'last_ok_at'));
    nextEl.textContent = fmtTime(earliest(sources, 'next_due_at'));
  }

  function playbackRow(s) {
    const total = Number(s.total_sec) || 0;
    const pos = Number(s.position_sec) || 0;
    const pct = total > 0 ? Math.min(100, Math.max(0, (pos / total) * 100)) : 0;
    // 只有播放列表请求、还没出过分片的会话是探测（MKACG-687）：照实列出来，
    // 但不计入「正在播放」的条数，行内标一下免得看起来像卡住的播放。
    const probing = s.state === 'probing';
    const parts = [];
    if (probing) parts.push('探测');
    if (s.unit) parts.push(s.unit);
    if (s.slices > 0) parts.push('第 ' + (Number(s.seq) + 1) + '/' + s.slices + ' 片');
    const client = [s.client_ip, s.user_agent].filter(Boolean).join(' · ') || '未知客户端';
    return el(
      'div',
      { class: 'play-item' },
      el('div', { class: 'play-item-top' }, el('div', { class: 'play-name', text: s.title || '未知剧目' }), el('div', { class: 'play-client', text: client })),
      el('div', { class: 'play-meta', text: parts.join(' · ') || '—' }),
      el('div', { class: 'progress' }, el('span', { style: 'width:' + pct.toFixed(1) + '%' })),
      el('div', { class: 'progress-labels' }, el('span', { text: fmtClock(pos) + ' / ' + fmtClock(total) }), el('span', { text: '已出 ' + fmtBytes(Number(s.bytes_served) || 0) }))
    );
  }

  function renderPlayback(pb) {
    const sessions = pb.sessions || [];
    const n = pb.streams || 0;
    const probing = pb.probing || 0;
    pbStreams.textContent = String(n);
    let summary = '没有客户端在拉流';
    if (n > 0) {
      summary = n + ' 条流 · ' + (pb.clients || 0) + ' 个客户端 · 已出 ' + fmtBytes(Number(pb.bytes) || 0);
    }
    if (probing > 0) {
      summary += (n > 0 ? ' · ' : '') + '探测中 ' + probing;
    }
    pbSummary.textContent = summary;
    pbBadge.textContent = n === 0 ? (probing > 0 ? '仅探测' : '空闲') : n + ' 条流';
    pbBadge.dataset.tone = n === 0 ? 'muted' : 'ok';
    pbEmpty.hidden = n + probing !== 0;
    pbList.replaceChildren(...sessions.map(playbackRow));
  }

  async function loadPlayback() {
    try {
      renderPlayback(await api('GET', '/api/playback'));
    } catch (err) {
      pbSummary.textContent = '读取失败：' + (err && err.message ? err.message : err);
      pbBadge.textContent = '读取失败';
      pbBadge.dataset.tone = 'danger';
      pbEmpty.hidden = false;
      pbList.replaceChildren();
    }
  }

  async function followPlayback() {
    if (playbackRunning) return;
    playbackRunning = true;
    try {
      for (;;) {
        if (document.hidden) break;
        await loadPlayback();
        await sleep(PLAYBACK_POLL_MS);
      }
    } finally {
      playbackRunning = false;
    }
  }

  async function load() {
    try {
      const st = await api('GET', '/api/status');
      renderSystem(st);
      renderSummary(st);
    } catch (err) {
      if (systemTitle) systemTitle.textContent = '系统状态读取失败';
      if (systemCopy) systemCopy.textContent = err && err.message ? err.message : String(err);
      if (systemBadge) setIcon('danger', '!');
      fail(err);
    }
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) followPlayback();
  });

  load();
  followPlayback();
}

/* ---------- 同步 ---------- */

function initSync() {
  const writeHint = document.getElementById('write-hint');
  const serverEl = document.getElementById('jellyfin-server');
  const badgeEl = document.getElementById('jellyfin-badge');
  const whenEl = document.getElementById('jellyfin-when');
  const lastEl = document.getElementById('jellyfin-last');
  const resultEl = document.getElementById('jellyfin-result');
  const progressEl = document.getElementById('jellyfin-progress');
  const issuesEl = document.getElementById('jellyfin-issues');
  const hintEl = document.getElementById('jellyfin-hint');
  const listEl = document.getElementById('jellyfin-categories');
  const syncBtn = document.getElementById('jellyfin-sync');
  const confirmDialog = document.getElementById('confirm-dialog');
  const confirmText = document.getElementById('confirm-text');
  const confirmOk = document.getElementById('confirm-ok');
  const confirmCancel = document.getElementById('confirm-cancel');

  const POLL_MS = 1000;
  const POLL_MAX_MS = 30 * 60 * 1000;

  let status = null;
  let categories = [];
  let genreCounts = new Map();
  let polling = false;

  const canWrite = () => Boolean(status && status.admin_token_set && adminToken() !== '');
  const jf = () => (status && status.jellyfin) || {};

  // 写操作可用性只在两处变化：令牌解锁状态、同步是否在跑。集中一处重算，
  // 列表里的开关与「立即同步」按钮才不会各算各的（同步期间一律禁用）。
  function updateControls() {
    const ready = canWrite();
    const running = jf().running === true;
    syncBtn.disabled = !ready || running;
    for (const sw of listEl.querySelectorAll('.switch')) {
      sw.disabled = !ready || running;
    }
  }

  function resultText(j) {
    const failed = (j.failed || []).length;
    const parts = ['新建 ' + (j.created || 0), '已存在 ' + (j.unchanged || 0), '删除 ' + (j.removed || 0)];
    if (failed > 0) parts.push('失败 ' + failed);
    if (typeof j.duration_ms === 'number' && j.duration_ms >= 0) parts.push('耗时 ' + fmtDuration(j.duration_ms));
    return parts.join(' · ');
  }

  // roundSucceeded 判「这一轮成不成功」：manager 的成功轮次用同一个时刻写 last_ok_at
  // 与 last_run_at（internal/jellyfin/manager.go 的 record），整轮中止、删库失败、
  // 建库超时只推进 last_run_at，last_ok_at 停在上一轮成功。两者相等才是完整成功。
  // last_error 只能当原因文案：同名库冲突、部分分类被拒也会写它，但那些轮次是成功的。
  // 页面上的成败结论（徽标、上次结果、进度行、收尾提示）都走这一个判据。
  function roundSucceeded(j) {
    return j.last_run_at !== '' && j.last_ok_at === j.last_run_at;
  }

  function renderSummary() {
    const j = jf();
    const configured = Boolean(j.configured);
    // ran = 至少跑过一轮（时间戳为空 = 从没跑过）；ok = 跑过且成功，判据见 roundSucceeded()。
    // 成败一律看这两个值，不看 last_error —— 它只用来写原因。
    const ran = Boolean(j.last_run_at);
    const ok = roundSucceeded(j);

    serverEl.textContent = configured ? j.url || '已配置（地址由部署提供）' : '服务器地址未配置';

    if (j.running) {
      badgeEl.textContent = '正在同步';
      badgeEl.dataset.tone = 'warn';
    } else if (!configured) {
      badgeEl.textContent = '未配置';
      badgeEl.dataset.tone = 'muted';
    } else if (ran && !ok) {
      badgeEl.textContent = '上次失败';
      badgeEl.dataset.tone = 'danger';
    } else {
      badgeEl.textContent = '连接正常';
      badgeEl.dataset.tone = 'ok';
    }

    whenEl.textContent = j.last_run_at ? fmtTime(j.last_run_at) : '还没有同步过';
    if (j.running) lastEl.textContent = '进行中';
    else if (ran && !ok) lastEl.textContent = '失败';
    else if (ran) lastEl.textContent = '成功';
    else lastEl.textContent = '—';
    resultEl.textContent = j.last_run_at || j.running ? resultText(j) : '—';

    if (j.running) {
      const stage = j.phase_label || j.phase || '准备中';
      const progress = j.total > 0 ? ' · ' + (j.processed || 0) + ' / ' + j.total : '';
      progressEl.textContent = '运行中 · ' + stage + progress + (j.started_at ? ' · 开始于 ' + fmtTime(j.started_at) : '');
      progressEl.dataset.kind = 'busy';
    } else if (ran && !ok) {
      progressEl.textContent = '上次同步失败：' + (j.last_error || '这一轮没有完成');
      progressEl.dataset.kind = 'error';
    } else if (ran) {
      progressEl.textContent = '上次同步：' + fmtTime(j.last_run_at) + ' · ' + resultText(j);
      progressEl.dataset.kind = 'ok';
    } else {
      progressEl.textContent = '';
      progressEl.dataset.kind = '';
    }

    const issues = [];
    for (const name of j.orphans || []) issues.push(['warn', '孤儿库（前缀匹配但目录已不存在）：' + name]);
    for (const name of j.conflicts || []) issues.push(['warn', '冲突库（同名但非托管）：' + name]);
    // 被拒的分类名只写在 last_error 里，而成功轮次不再展示 last_error（它不再是成败判据）。
    // 这里点名，免得「失败 N」只有计数、没有对象。
    const rejected = j.failed || [];
    if (rejected.length > 0) {
      issues.push(['warn', 'Jellyfin 拒绝了这些分类的建库（多半是库路径在 Jellyfin 容器内不存在）：' + rejected.join('、')]);
    }
    issuesEl.replaceChildren(...issues.map(([tone, text]) => el('p', { class: 'alert', 'data-tone': tone, text })));

    if (!status.admin_token_set) hintEl.textContent = '部署时未设置 DRAMA_HUB_ADMIN_TOKEN：写操作一律 401。';
    else if (adminToken() === '') hintEl.textContent = '写操作需要先解锁管理令牌（右上角）。';
    else if (!configured) hintEl.textContent = '先在设置页填好 Jellyfin 服务器地址与 API Key，再回来同步。';
    else hintEl.textContent = '只创建与维护由 drama-hub 管理的媒体库，不会改动用户手动创建的库。';

    if (writeHint) {
      if (!status.admin_token_set) writeHint.textContent = '部署时未设置 DRAMA_HUB_ADMIN_TOKEN：写操作一律 401。';
      else if (adminToken() === '') writeHint.textContent = '写操作需要先解锁管理令牌（右上角）。';
      else writeHint.textContent = '';
    }

    updateControls();
  }

  function libraryRow(item) {
    const count = genreCounts.get(item.name) || 0;
    const switchBtn = el('button', {
      type: 'button',
      class: 'switch',
      role: 'switch',
      'aria-checked': item.enabled ? 'true' : 'false',
      'aria-label': (item.enabled ? '关闭「' : '开启「') + item.name + '」媒体库同步',
    });
    switchBtn.addEventListener('click', () => toggleCategory(item.name, item.enabled, switchBtn));

    return el(
      'div',
      { class: 'library-row', 'data-enabled': item.enabled ? 'true' : 'false' },
      el('div', {}, el('div', { class: 'library-name', text: item.name }), el('div', { class: 'library-count', text: count + ' 部短剧' })),
      el(
        'div',
        { class: 'library-state' },
        el('span', { class: 'dot', 'data-tone': item.enabled ? 'ok' : 'muted' }),
        el('span', { text: item.enabled ? '随同步维护' : '下次同步删除' })
      ),
      el('div', { class: 'switch-cell' }, el('span', { class: 'switch-label', text: item.enabled ? '已启用' : '已关闭' }), switchBtn)
    );
  }

  function renderCategories() {
    if (categories.length === 0) {
      listEl.replaceChildren(el('p', { class: 'empty', text: '还没有可管理的分类媒体库。先让播放源扫描出分类，再回来开启。' }));
    } else {
      listEl.replaceChildren(...categories.map(libraryRow));
    }
    updateControls();
  }

  async function askClose(name) {
    if (!confirmDialog) return true;
    confirmText.textContent = '关闭「' + name + '」后，对应的托管媒体库会在下一轮周期同步或点「立即同步」时从 Jellyfin 删除。用户手动创建的媒体库不受影响。';
    return new Promise((resolve) => {
      let settled = false;
      const finish = (value) => {
        if (settled) return;
        settled = true;
        confirmOk.removeEventListener('click', onOk);
        confirmCancel.removeEventListener('click', onCancel);
        confirmDialog.removeEventListener('close', onClose);
        if (confirmDialog.open) confirmDialog.close();
        resolve(value);
      };
      const onOk = () => finish(true);
      const onCancel = () => finish(false);
      const onClose = () => finish(false);
      confirmOk.addEventListener('click', onOk);
      confirmCancel.addEventListener('click', onCancel);
      confirmDialog.addEventListener('close', onClose);
      if (typeof confirmDialog.showModal === 'function') confirmDialog.showModal();
      else confirmDialog.setAttribute('open', '');
    });
  }

  async function toggleCategory(name, enabled, button) {
    // 关闭媒体库会真的删掉 Jellyfin 里的库，先二次确认；开启是幂等重建，直接执行。
    if (enabled) {
      const ok = await askClose(name);
      if (!ok) return;
    }
    button.disabled = true;
    try {
      const data = await api('PATCH', '/api/jellyfin/categories', { name, enabled: !enabled });
      categories = categories.map((c) => (c.name === name ? { name: c.name, enabled: Boolean(data.enabled) } : c));
      renderCategories();
      say(enabled ? '已关闭「' + name + '」：下一轮同步会删除它的媒体库' : '已开启「' + name + '」：下一轮同步会重建它的媒体库', 'ok');
    } catch (err) {
      updateControls();
      fail(err);
    }
  }

  async function loadStatus() {
    status = await api('GET', '/api/status');
    renderSummary();
  }

  async function loadCategories() {
    const data = await api('GET', '/api/jellyfin/categories');
    categories = data.categories || [];
    try {
      const list = await api('GET', '/api/dramas');
      const counts = new Map();
      for (const drama of list.dramas || []) {
        const key = genreDir(drama.genre);
        counts.set(key, (counts.get(key) || 0) + 1);
      }
      genreCounts = counts;
    } catch (err) {
      genreCounts = new Map();
    }
    renderCategories();
  }


  // expectRun：本次调用之前是否真的提交了一轮同步（POST 成功或 409 已在运行）。
  // 那一轮可能在我们拿到状态之前就跑完了，此时也该补一条结果，而不是留着「已开始同步」。
  async function follow(expectRun) {
    if (polling) return;
    polling = true;
    const started = Date.now();
    // 只有本轮确实跑过（看见过运行中，或 POST 已被接受）才由这里下结论。
    // POST 被拒（400/401）、轮询失败、超 30 分钟这三种情况各自留着原来的提示，
    // 不能被收尾逻辑改写成「同步完成」。
    let sawRunning = false;
    let concluded = false;
    try {
      for (;;) {
        if (!jf().running) return;
        sawRunning = true;
        if (Date.now() - started > POLL_MAX_MS) {
          say('同步超过 30 分钟仍未结束，已停止自动刷新进度；稍后重新打开本页查看结果', 'error');
          concluded = true;
          return;
        }
        await sleep(POLL_MS);
        try {
          const j = await api('GET', '/api/jellyfin/libraries');
          status.jellyfin = j;
          renderSummary();
        } catch (err) {
          fail(err);
          concluded = true;
          return;
        }
      }
    } finally {
      polling = false;
      await load().catch(fail);
      // 收尾只处理「本轮真的跑过、但还没人说过结果」这一种情况：POST 被拒、
      // 轮询失败、超 30 分钟都已在上面写过各自的结论，不能被这里改写。
      if ((sawRunning || expectRun === true) && !concluded) {
        const j = jf();
        if (roundSucceeded(j)) say('同步完成：' + resultText(j), 'ok');
        else say('同步失败：' + (j.last_error || '这一轮没有完成'), 'error');
      }
    }
  }

  async function runSync() {
    if (syncBtn.disabled) return;
    syncBtn.disabled = true;
    syncBtn.textContent = '正在同步…';
    let accepted = false;
    try {
      await api('POST', '/api/jellyfin/libraries', {});
      accepted = true;
      say('已开始同步，进度会显示在这一页', 'busy');
    } catch (err) {
      if (err.status === 409) {
        accepted = true;
        say('同步已在运行，接着显示这一轮的进度', 'busy');
      } else {
        fail(err);
      }
    } finally {
      syncBtn.textContent = '立即同步';
      await loadStatus().catch(fail);
      await follow(accepted).catch(fail);
    }
  }

  async function load() {
    await loadStatus();
    renderSummary();
    await loadCategories();
  }

  syncBtn.addEventListener('click', runSync);
  // 解锁/清除令牌后立刻重算开关与按钮，不必刷新页面。
  document.addEventListener(TOKEN_EVENT, () => {
    if (status) renderSummary();
  });
  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', () => {
    if (status) renderSummary();
  });

  load()
    .then(() => {
      if (jf().running) return follow(true);
    })
    .catch(fail);
}

/* ---------- 设置 ---------- */

function initSettings() {
  const form = document.getElementById('settings-form');
  const saveBtn = document.getElementById('settings-save');
  const hint = document.getElementById('settings-hint');
  const readonlyBox = document.getElementById('readonly');
  if (!form) return;

  const fields = {
    strm_prefix: 'set-strm-prefix',
    cache_max_mb: 'set-cache-max-mb',
    cache_lru_days: 'set-cache-lru-days',
    merge_chunk_size: 'set-merge-chunk-size',
    refresh_interval_sec: 'set-refresh-interval',
  };
  const credentials = {
    webdav_user: 'set-webdav-user',
    webdav_pass: 'set-webdav-pass',
  };
  const jellyfinFields = [
    { key: 'jellyfin_url', id: 'set-jellyfin-url', view: 'url' },
    { key: 'jellyfin_library_root', id: 'set-jellyfin-root', view: 'library_root' },
    { key: 'jellyfin_library_prefix', id: 'set-jellyfin-prefix', view: 'library_prefix' },
    { key: 'jellyfin_timeout_sec', id: 'set-jellyfin-timeout', view: 'timeout_sec' },
  ];
  // 成功提示用页面上的字段名，不把 settings 内部键名直接抛给用户。
  const fieldLabels = {
    webdav_user: 'WebDAV 用户名',
    webdav_pass: 'WebDAV 密码',
    strm_prefix: 'STRM 地址前缀',
    refresh_interval_sec: '刷新间隔',
    merge_chunk_size: '合并集包含集数',
    cache_max_mb: '缓存容量上限',
    cache_lru_days: '缓存保留天数',
    jellyfin_url: 'Jellyfin 服务器地址',
    jellyfin_api_key: 'Jellyfin API Key',
    jellyfin_library_root: '媒体库根目录',
    jellyfin_library_prefix: '库名前缀',
    jellyfin_timeout_sec: 'Jellyfin 请求超时',
  };

  const jellyfinBaseline = {};
  const baseline = {};
  let latest = null;
  let prefilled = false;
  let adminWritable = false;

  const canWrite = () => adminWritable && adminToken() !== '';

  function fillReadonly(c, j) {
    const rows = [
      ['缓存目录', c.cache_dir || '—'],
      ['每轮存量部数', String(c.refresh_batch) + '（0 = 关闭存量轮转）'],
      ['单轮预算', String(c.refresh_timeout_sec) + ' 秒（超时后下一轮按游标续跑）'],
      ['出流签名密钥', c.stream_secret_set ? '已设置' : '未设置'],
      ['WebDAV 凭据', c.webdav_user_set && c.webdav_pass_set ? '已设置' : '未设置（/webdav 恒 401）'],
      ['Jellyfin API Key', j.api_key_set ? '已设置' : '未设置'],
    ];
    readonlyBox.replaceChildren(...rows.map(([k, v]) => el('div', {}, el('dt', { text: k }), el('dd', { text: v }))));
  }

  function fillSettings(c) {
    for (const [key, id] of Object.entries(fields)) {
      const input = document.getElementById(id);
      if (!input) continue;
      baseline[key] = String(c[key]);
      input.value = baseline[key];
    }
    setCredentialPlaceholders(c);
  }

  function setCredentialPlaceholders(c) {
    const user = document.getElementById(credentials.webdav_user);
    const pass = document.getElementById(credentials.webdav_pass);
    if (user) {
      user.value = '';
      user.placeholder = c.webdav_user_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
    }
    if (pass) {
      pass.value = '';
      pass.placeholder = c.webdav_pass_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
    }
  }

  function fillJellyfin(j) {
    for (const field of jellyfinFields) {
      const input = document.getElementById(field.id);
      if (!input) continue;
      jellyfinBaseline[field.key] = String(j[field.view]);
      input.value = jellyfinBaseline[field.key];
    }
    setJellyfinKeyPlaceholder(j);
  }

  function setJellyfinKeyPlaceholder(j) {
    const input = document.getElementById('set-jellyfin-api-key');
    if (!input) return;
    input.value = '';
    input.placeholder = j.api_key_set ? '已设置，留空不改' : '未设置（这一段不启用）';
  }

  function updateHint() {
    if (!adminWritable) {
      saveBtn.disabled = true;
      hint.textContent = '部署时未设置 DRAMA_HUB_ADMIN_TOKEN：写操作一律 401。';
      return;
    }
    saveBtn.disabled = adminToken() === '';
    hint.textContent = adminToken() === '' ? '写操作需要先解锁管理令牌（右上角）。' : '保存只会发送改动的项；配好令牌后可用。';
  }

  async function load() {
    const st = await api('GET', '/api/status');
    latest = st;
    adminWritable = st.admin_token_set === true;
    if (!prefilled) {
      fillSettings(st.config);
      fillJellyfin(st.jellyfin);
      prefilled = true;
    } else {
      setCredentialPlaceholders(st.config);
      setJellyfinKeyPlaceholder(st.jellyfin);
    }
    fillReadonly(st.config, st.jellyfin);
    updateHint();
  }

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const payload = {};
    for (const [key, id] of Object.entries(fields)) {
      const input = document.getElementById(id);
      if (!input) continue;
      const value = input.value.trim();
      if (value !== baseline[key]) payload[key] = value;
    }
    for (const [key, id] of Object.entries(credentials)) {
      const input = document.getElementById(id);
      if (!input) continue;
      const value = input.value.trim();
      if (value !== '') payload[key] = value;
    }
    for (const field of jellyfinFields) {
      const input = document.getElementById(field.id);
      if (!input) continue;
      const value = input.value;
      if (value !== jellyfinBaseline[field.key]) payload[field.key] = value;
    }
    const keyInput = document.getElementById('set-jellyfin-api-key');
    if (keyInput && keyInput.value.trim() !== '') payload.jellyfin_api_key = keyInput.value.trim();

    if (Object.keys(payload).length === 0) {
      say('没有改动', 'ok');
      return;
    }

    saveBtn.disabled = true;
    say('正在写入配置 …', 'busy');
    try {
      const data = await api('PATCH', '/api/settings', { settings: payload });
      for (const [key, id] of Object.entries(fields)) {
        const input = document.getElementById(id);
        if (!input || !(key in data.settings)) continue;
        baseline[key] = String(data.settings[key]);
        input.value = baseline[key];
      }
      for (const field of jellyfinFields) {
        const input = document.getElementById(field.id);
        if (!input || !(field.key in data.settings)) continue;
        jellyfinBaseline[field.key] = String(data.settings[field.key]);
        input.value = jellyfinBaseline[field.key];
      }
      for (const id of Object.values(credentials)) {
        const input = document.getElementById(id);
        if (input) input.value = '';
      }
      if (keyInput) keyInput.value = '';
      say('已写入：' + Object.keys(data.settings).map((key) => fieldLabels[key] || key).join('、') + '（生效时机见各字段徽标）', 'ok');
      await load();
    } catch (err) {
      fail(err);
    } finally {
      updateHint();
    }
  });

  // 「重置」= 放弃未保存的改动，回到服务器当前值。
  form.addEventListener('reset', (ev) => {
    ev.preventDefault();
    if (!latest) return;
    fillSettings(latest.config);
    fillJellyfin(latest.jellyfin);
    updateHint();
    say('已恢复为服务器当前值', 'ok');
  });

  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', updateHint);
  document.addEventListener(TOKEN_EVENT, updateHint);

  load().catch(fail);
}

/* ---------- 挂载 ---------- */

initAdminToken();
initTokenDialog();

const page = document.body.dataset.page;
if (page === 'overview') initOverview();
else if (page === 'sync') initSync();
else if (page === 'settings') initSettings();
