'use strict';
/* drama-hub 管理面脚本：通用请求/渲染小工具 + 两个页面的 init，靠 body[data-page] 分派。
 * 只用原生 DOM、不引框架、没有构建步骤（ADR-0002）；数据一律来自 /api/ 的 JSON。 */

// ---- 通用 ----

// 写操作带 X-Admin-Token（env DRAMA_HUB_ADMIN_TOKEN）；令牌只存 sessionStorage，关标签页即失效。
const TOKEN_KEY = 'drama-hub.admin-token';
const TOKEN_HEADER = 'X-Admin-Token';

/** 当前管理令牌：以顶栏输入框为准（刚打完字、还没触发 change 时也拿得到）。 */
function adminToken() {
  const input = document.getElementById('admin-token');
  if (input && input.value.trim() !== '') return input.value.trim();
  return sessionStorage.getItem(TOKEN_KEY) || '';
}

/** 顶栏令牌框：回填上次输入，改动即记住（清空 = 忘掉）。 */
function initAdminToken() {
  const input = document.getElementById('admin-token');
  if (!input) return;
  input.value = sessionStorage.getItem(TOKEN_KEY) || '';
  input.addEventListener('change', () => {
    const value = input.value.trim();
    input.value = value;
    if (value === '') sessionStorage.removeItem(TOKEN_KEY);
    else sessionStorage.setItem(TOKEN_KEY, value);
  });
}

/** 请求管理 API，返回 {status, data}；非 2xx 抛 Error（带服务端 message）。 */
async function apiRaw(method, path, body) {
  const init = { method, headers: { Accept: 'application/json' } };
  if (method !== 'GET') init.headers[TOKEN_HEADER] = adminToken();
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let data = null;
  if (text !== '') {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const message = data && data.message ? data.message : res.status + ' ' + res.statusText;
    const error = new Error(message);
    error.status = res.status; // 409 这类「不是故障的状态」页面要单独处理（见 syncJellyfin）
    throw error;
  }
  return { status: res.status, data };
}

/** 只要响应体：绝大多数调用不关心 200 / 201 的区别。 */
async function api(method, path, body) {
  return (await apiRaw(method, path, body)).data;
}

/** 建节点：props 里 text 走 textContent，on* 走事件，其余走 setAttribute。 */
function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null) continue;
    if (key === 'text') node.textContent = value;
    else if (key === 'class') node.className = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, String(value));
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

/** 状态徽标：tone = ok / warn / danger / muted。 */
function badge(text, tone) {
  return el('span', { class: 'badge', 'data-tone': tone || null, text });
}

/** 在某个 live region 上显示一条提示（默认页面级 #notice）。 */
function say(text, kind, id) {
  const box = document.getElementById(id || 'notice');
  if (!box) return;
  box.textContent = text;
  box.dataset.kind = kind || '';
}

/** 统一的失败出口：错误文案来自响应体，直接显示。 */
function fail(err) {
  say(err && err.message ? err.message : String(err), 'error');
}

/** 把 UTC RFC3339 文本按浏览器本地时区显示；空值显示破折号。 */
function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
    ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

/** 把毫秒写成「不到 1 秒 / 12.3 秒 / 1 分 5 秒」；非数字显示破折号。 */
function fmtDuration(ms) {
  if (typeof ms !== 'number' || ms < 0) return '—';
  if (ms < 1000) return '不到 1 秒';
  const sec = ms / 1000;
  if (sec < 60) return sec.toFixed(1) + ' 秒';
  return Math.floor(sec / 60) + ' 分 ' + Math.round(sec % 60) + ' 秒';
}

/** 等一会儿（轮询间隔用）。 */
function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// ---- 概览 ----

function initOverview() {
  const summaryBox = document.getElementById('summary');
  const sourcesBox = document.getElementById('sources');
  const writeHint = document.getElementById('write-hint');
  const jfBadge = document.getElementById('jellyfin-badge');
  const jfWhen = document.getElementById('jellyfin-when');
  const jfIssues = document.getElementById('jellyfin-issues');
  const jfProgress = document.getElementById('jellyfin-progress');
  const jfBtn = document.getElementById('jellyfin-sync');
  const jfHint = document.getElementById('jellyfin-hint');
  const jfCategories = document.getElementById('jellyfin-categories');
  const dramaForm = document.getElementById('drama-form');
  const dramaInput = document.getElementById('series-id');
  const dramaBtn = document.getElementById('drama-submit');
  const pbSummary = document.getElementById('playback-summary');
  const pbBadge = document.getElementById('playback-badge');
  const pbList = document.getElementById('playback-list');
  const pbEmpty = document.getElementById('playback-empty');

  let status = null; // 最近一次 /api/status：令牌改动后据此重画按钮，不必再打服务端
  let jellyfinPolling = false;
  let playbackPolling = false;

  const canWrite = () => Boolean(status && status.admin_token_set && adminToken() !== '');

  function renderSummary(t) {
    summaryBox.replaceChildren(
      el('span', {}, el('b', { text: String(t.dramas) }), ' 部剧'),
      el('span', {}, el('b', { text: String(t.episodes) }), ' 集'),
      el('span', {}, el('b', { text: String(t.emby_items) }), ' 个对外集'),
      el('span', {}, '最近同步 ', el('b', { text: fmtTime(t.last_sync_at) })));
  }

  // refreshSource 打手动触发端点；202 / 409 / 401 / 404 之后一律回读 /api/status，不猜结局。
  async function refreshSource(src, button) {
    button.disabled = true;
    say('已请求刷新 ' + src.name + ' …', 'busy');
    try {
      await api('POST', '/api/refresh', { source: src.name });
      say('已派出 ' + src.name + ' 的一轮刷新，结果看「上次运行」与「上次错误」', 'ok');
    } catch (err) {
      fail(err);
    }
    await load();
  }

  function sourceCard(src) {
    const name = src.label === src.name ? src.name : src.label;
    const state = src.running
      ? badge('正在刷新', 'warn')
      : src.enabled ? badge('已启用', 'ok') : badge('已停用', 'danger');
    const meta = name === src.name ? [] : [el('p', { class: 'card-sub', text: src.name })];
    const body = el('div', { class: 'card-body' },
      el('dl', { class: 'kv' },
        el('dt', { text: '上次运行' }), el('dd', { title: src.last_run_at, text: fmtTime(src.last_run_at) }),
        el('dt', { text: '上次成功' }), el('dd', { title: src.last_ok_at, text: fmtTime(src.last_ok_at) }),
        el('dt', { text: '下次计划' }), el('dd', { title: src.next_due_at, text: fmtTime(src.next_due_at) })));
    if (src.last_error !== '') {
      body.append(el('p', { class: 'alert' }, el('b', { text: '上次错误' }), el('span', { text: src.last_error })));
    }
    if (src.reason !== '') {
      body.append(el('p', { class: 'alert', 'data-tone': 'warn' }, el('span', { text: src.reason })));
    }
    if (src.restart_required) {
      // base_url / 超时是启动时注入插件的构造期配置：改了要重启，页面上直说。
      body.append(el('p', { class: 'hint', text:
        'base_url 生效值 ' + (src.base_url_effective === '' ? '插件内置默认' : src.base_url_effective) +
        '，请求超时生效值 ' + src.request_timeout_sec_effective + ' 秒；settings 现值（base_url ' +
        (src.base_url === '' ? '空' : src.base_url) + ' / 超时 ' + src.request_timeout_sec +
        ' 秒）重启后生效。' }));
    }
    body.append(el('p', { class: 'hint', text: '刷新间隔 ' + src.interval_sec + ' 秒。' }));
    const button = el('button', {
      type: 'button', class: 'btn', text: '立即刷新',
      disabled: !src.enabled,
      onclick: (event) => refreshSource(src, event.currentTarget),
    });
    button.dataset.refresh = src.name;
    return el('article', { class: 'card' },
      el('header', { class: 'card-head' },
        el('div', {}, el('h2', { text: name }), meta), state),
      body,
      el('footer', { class: 'card-foot' }, button));
  }

  function renderSources(sources) {
    if (sources.length === 0) {
      sourcesBox.replaceChildren(el('p', { class: 'state', 'data-state': 'empty',
        text: '还没有注册播放源；在服务端配置源插件后重启。' }));
      return;
    }
    sourcesBox.replaceChildren(...sources.map(sourceCard));
  }

  function renderJellyfin(j) {
    const missing = j.missing.length === 0 ? '' : ' · 缺 ' + j.missing.join('、');
    const text = j.running
      ? '正在同步'
      : !j.configured
        ? '未配置'
        : j.last_error !== '' ? '上次失败' : '已启用';
    const tone = j.running ? 'warn' : !j.configured ? 'muted' : j.last_error !== '' ? 'danger' : 'ok';
    jfBadge.textContent = text + missing;
    jfBadge.dataset.tone = tone;
    jfWhen.textContent = j.last_run_at === '' ? '还没有同步过' : '上次同步 ' + fmtTime(j.last_run_at);

    const groups = [
      ['孤儿库', j.orphans],
      ['冲突库', j.conflicts],
      ['被拒分类', j.failed],
    ].filter(([, list]) => list.length > 0);
    jfIssues.replaceChildren(...groups.map(([label, list]) =>
      el('div', { class: 'issue' },
        el('h3', { text: label }),
        el('ul', {}, ...list.map((item) => el('li', { text: item }))))));
  }

  // jellyfinResultText 把「这一轮做了什么」写成一句话（MKACG-664 AC-2）。
  // 「更新」恒 0：已存在的库一律不改（不重命名、不改路径，幂等）。
  function jellyfinResultText(j) {
    let text = '新建 ' + j.created + '、更新 0（已存在的库不改动）、已存在 ' + j.unchanged +
      '、删除 ' + j.removed + '、失败 ' + j.failed.length +
      ' · 耗时 ' + fmtDuration(j.duration_ms);
    if (j.last_error !== '') text += ' · ' + j.last_error;
    return text;
  }

  // ---- 分类开关（MKACG-668）----
  //
  // 开关只是一个 PATCH：只改库、不碰 Jellyfin。关掉的分类在下一轮同步（周期或
  // 「同步媒体库」）里删库，所以这里的文案一律说「下一轮」，不说「已删除」。

  function categoryRow(item) {
    const input = el('input', {
      type: 'checkbox', class: 'switch',
      'aria-label': item.name + ' 同步到 Jellyfin',
    });
    input.checked = item.enabled;
    const stateText = el('span', { class: 'switch-state', text: item.enabled ? '已开启' : '已关闭' });
    input.addEventListener('change', () => toggleCategory(item.name, input, stateText));
    return el('div', { class: 'switch-row', 'data-enabled': String(item.enabled) },
      el('label', {}, input, el('span', { class: 'switch-name', text: item.name })),
      stateText);
  }

  function renderCategories(categories) {
    if (!categories || categories.length === 0) {
      jfCategories.replaceChildren(el('p', { class: 'state', 'data-state': 'empty',
        text: '还没有分类；库里先有剧集才会出现分类。' }));
      return;
    }
    jfCategories.replaceChildren(...categories.map(categoryRow));
    updateCategoryWritable();
  }

  async function loadCategories() {
    try {
      const data = await api('GET', '/api/jellyfin/categories');
      renderCategories(data.categories);
    } catch (err) {
      jfCategories.replaceChildren(el('p', { class: 'state', 'data-state': 'error', text: err.message }));
    }
  }

  async function toggleCategory(name, input, stateText) {
    const enabled = input.checked;
    input.disabled = true;
    try {
      await api('PATCH', '/api/jellyfin/categories', { name, enabled });
      stateText.textContent = enabled ? '已开启' : '已关闭';
      input.closest('.switch-row').dataset.enabled = String(enabled);
      say(enabled
        ? '已开启「' + name + '」：下一轮同步会重建它的媒体库'
        : '已关闭「' + name + '」：下一轮同步会删掉它的媒体库', 'ok');
    } catch (err) {
      input.checked = !enabled; // 库里的状态没变，输入框跟着回滚
      fail(err);
    } finally {
      input.disabled = !canWrite();
    }
  }

  function updateCategoryWritable() {
    const ready = canWrite();
    for (const input of jfCategories.querySelectorAll('input')) input.disabled = !ready;
  }

  // 664：运行中显示阶段与进度；收尾后显示上次同步时间与结果。
  function renderJellyfinProgress(j) {
    if (j.running) {
      const parts = ['运行中'];
      if (j.phase_label !== '') parts.push('阶段：' + j.phase_label);
      if (j.total > 0) parts.push(j.processed + ' / ' + j.total);
      if (j.started_at !== '') {
        parts.push('开始于 ' + fmtTime(j.started_at));
        parts.push('已用 ' + fmtDuration(Date.now() - Date.parse(j.started_at)));
      }
      jfProgress.textContent = parts.join(' · ');
      jfProgress.dataset.kind = 'busy';
      return;
    }
    jfProgress.dataset.kind = j.last_error === '' ? 'ok' : 'error';
    jfProgress.textContent = j.last_run_at === ''
      ? ''
      : '上次同步：' + fmtTime(j.last_run_at) + ' · ' + jellyfinResultText(j);
  }

  const JELLYFIN_POLL_MS = 1000;
  const JELLYFIN_POLL_MAX_MS = 30 * 60 * 1000;

  async function followJellyfin() {
    if (jellyfinPolling) return; // 同一时刻只留一条轮询链（load 与按钮都会进来）
    jellyfinPolling = true;
    const deadline = Date.now() + JELLYFIN_POLL_MAX_MS;
    try {
      for (;;) {
        const j = await api('GET', '/api/jellyfin/libraries');
        renderJellyfin(j);
        renderJellyfinProgress(j);
        if (!j.running) break;
        if (Date.now() > deadline) {
          say('媒体库同步超过 ' + JELLYFIN_POLL_MAX_MS / 60000 + ' 分钟仍未收尾，已停止自动刷新；' +
            '再点一次「同步媒体库」会返回「已在运行」并接着显示进度', 'busy');
          return;
        }
        await sleep(JELLYFIN_POLL_MS);
      }
    } catch (err) {
      fail(err);
    } finally {
      jellyfinPolling = false;
    }
    // 收尾后再读一次状态：updateWritable 用最新的 running 把「同步媒体库」
    // 等写操作按钮放回来。（超时 return 的路径仍在跑，不刷新。）
    await load().catch(fail);
  }

  async function syncJellyfin() {
    jfBtn.disabled = true;
    say('已请求同步 Jellyfin 媒体库 …', 'busy');
    try {
      await api('POST', '/api/jellyfin/libraries', {});
      say('', ''); // 进度与结果在卡片里，notice 不留一句立刻过时的话
    } catch (err) {
      // 409 不是故障：单飞锁挡住了重复派发，页面说「已在运行」而不是报红色错误。
      if (err.status === 409) say('Jellyfin 媒体库同步已在运行，接着显示这一轮的进度', 'busy');
      else fail(err);
    } finally {
      jfBtn.disabled = false;
      await load().catch(fail);
      await followJellyfin();
    }
  }

  // ---- 正在播放（MKACG-683）----

  const PLAYBACK_POLL_MS = 4000; // 页面刷新 ≤5 s，且只在可见时发请求

  /** 把字节写成 B / KB / MB / GB。 */
  function fmtBytes(n) {
    if (typeof n !== 'number' || n < 0) return '—';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
  }

  /** 把秒写成「分:秒」。 */
  function fmtClock(sec) {
    const s = Math.max(0, Math.floor(sec || 0));
    return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
  }

  function playbackRow(s) {
    const title = s.title === '' ? '未知剧目' : s.title;
    const unit = s.unit === '' ? '未知块' : s.unit;
    const seq = s.slices > 0 ? '第 ' + (s.seq + 1) + '/' + s.slices + ' 片' : '第 ' + (s.seq + 1) + ' 片';
    const pos = s.total_sec > 0
      ? fmtClock(s.position_sec) + '/' + fmtClock(s.total_sec)
      : fmtClock(s.position_sec);
    return el('article', { class: 'playback-row' },
      el('div', { class: 'playback-head' },
        el('strong', { text: title }),
        el('span', { class: 'card-sub', text: unit + ' · ' + seq })),
      el('dl', { class: 'kv' },
        el('dt', { text: '进度' }), el('dd', { text: pos }),
        el('dt', { text: '已出' }), el('dd', { text: fmtBytes(s.bytes_served) }),
        el('dt', { text: '客户端' }), el('dd', { text: s.client_ip === '' ? '—' : s.client_ip }),
        el('dt', { text: 'UA' }), el('dd', { title: s.user_agent, text: s.user_agent === '' ? '—' : s.user_agent }),
        el('dt', { text: '最后活跃' }), el('dd', { title: s.last_seen, text: fmtTime(s.last_seen) })));
  }

  function renderPlayback(pb) {
    const n = pb.streams;
    pbSummary.textContent = n === 0
      ? '没有客户端在拉流'
      : n + ' 条流 · ' + pb.clients + ' 个客户端 · 已出 ' + fmtBytes(pb.bytes);
    pbBadge.textContent = n === 0 ? '空闲' : n + ' 条流';
    pbBadge.dataset.tone = n === 0 ? 'muted' : 'ok';
    pbEmpty.hidden = n !== 0;
    pbList.replaceChildren(...pb.sessions.map(playbackRow));
  }

  async function loadPlayback() {
    try {
      renderPlayback(await api('GET', '/api/playback'));
    } catch (err) {
      // 快照读失败只影响这一块：页面其余部分照常。
      pbSummary.textContent = '读取失败';
      pbBadge.textContent = '失败';
      pbBadge.dataset.tone = 'danger';
      pbEmpty.hidden = true;
      pbList.replaceChildren(el('p', { class: 'state', 'data-state': 'error', text: err.message }));
    }
  }

  // 轮询链只有一个：每 4 s 读一次快照；页面不可见时不发请求，切回来立刻续上。
  async function followPlayback() {
    if (playbackPolling) return;
    playbackPolling = true;
    try {
      for (;;) {
        if (document.hidden) break;
        await loadPlayback();
        await sleep(PLAYBACK_POLL_MS);
      }
    } finally {
      playbackPolling = false;
    }
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) followPlayback();
  });

  function updateWritable() {
    if (!status) return;
    const ready = canWrite();
    if (!status.admin_token_set) {
      writeHint.textContent = '未设置 DRAMA_HUB_ADMIN_TOKEN：刷新、同步与改配置都会 401。在服务端加上这个 env 再重启。';
    } else if (adminToken() === '') {
      writeHint.textContent = '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再操作（令牌只存在本标签页）。';
    } else {
      writeHint.textContent = '';
    }
    jfBtn.disabled = !ready || status.jellyfin.running;
    dramaBtn.disabled = !ready;
    updateCategoryWritable();
    for (const button of sourcesBox.querySelectorAll('button[data-refresh]')) {
      button.disabled = !ready;
      if (status.sources.some((src) => src.name === button.dataset.refresh && (!src.enabled || src.running))) {
        button.disabled = true;
      }
    }
    jfHint.textContent = !status.jellyfin.configured
      ? '还缺 ' + status.jellyfin.missing.join('、') + '；地址 / API key / 媒体库根三项都非空才启用。'
      : ready
        ? '关掉的分类在下一轮同步时删库；孤儿库（分类已消失）只报告，不删。'
        : '同步媒体库与分类开关都是写操作，需要先在顶栏填管理令牌。';
  }

  async function load() {
    try {
      status = await api('GET', '/api/status');
    } catch (err) {
      summaryBox.textContent = '读取失败';
      sourcesBox.replaceChildren(el('p', { class: 'state', 'data-state': 'error' },
        err.message + ' ',
        el('button', { type: 'button', class: 'btn', text: '重试', onclick: () => load() })));
      fail(err);
      return;
    }
    renderSummary(status.totals);
    renderSources(status.sources);
    renderJellyfin(status.jellyfin);
    renderJellyfinProgress(status.jellyfin);
    updateWritable();
    // 分类开关是独立的一次读：只在页面打开 / 每轮 load 时取，不进每秒的进度轮询。
    loadCategories();
    // 刷新 / 首次打开时这一轮可能还在跑：接着看，不必等用户再点一次。
    if (status.jellyfin.running) followJellyfin();
  }

  dramaForm.addEventListener('submit', async (event) => {
    event.preventDefault();
    const raw = dramaInput.value.trim();
    if (raw === '') {
      say('先填上游剧 id 或 /player/ 链接', 'error', 'drama-notice');
      return;
    }
    const digits = raw.match(/\d+/g);
    const sid = digits === null ? '' : digits[digits.length - 1];
    dramaBtn.disabled = true;
    say('正在从上游同步 …', 'busy', 'drama-notice');
    try {
      const { status: code, data } = await apiRaw('POST', '/api/dramas', { series_id: raw });
      const hit = data && data.dramas ? data.dramas.find((d) => d.provider_id === sid) : null;
      const title = hit ? '《' + hit.title + '》' : '这部剧';
      const extra = hit ? '，库里 ' + hit.episodes + ' 集' : '';
      say(code === 201
        ? '已新建 ' + title + extra + '（201）'
        : title + ' 已在库，这次只更新' + extra + '（200）', 'ok', 'drama-notice');
      dramaInput.value = '';
      await load();
    } catch (err) {
      say(err.message, 'error', 'drama-notice');
    } finally {
      dramaBtn.disabled = !canWrite();
    }
  });

  jfBtn.addEventListener('click', syncJellyfin);

  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', updateWritable);

  load();
  followPlayback();
}

// ---- 设置 ----

function initSettings() {
  const form = document.getElementById('settings-form');
  const saveBtn = document.getElementById('settings-save');
  const hint = document.getElementById('settings-hint');
  const readonlyBox = document.getElementById('readonly');

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
  let jellyfinBaseline = {};
  let baseline = {}; // 提交时只发改过的项
  let prefilled = false; // 只在首次加载与保存成功后回填，不冲掉正在打的值
  let adminWritable = false; // 服务端有没有配 DRAMA_HUB_ADMIN_TOKEN

  const canWrite = () => adminWritable && adminToken() !== '';

  function flag(set, yes, no) {
    return el('span', { class: 'badge', 'data-tone': set ? 'ok' : 'muted', text: set ? yes : no });
  }

  function renderReadonly(c, j) {
    readonlyBox.replaceChildren(
      el('dt', { text: '缓存目录' }), el('dd', { class: 'mono', text: c.cache_dir }),
      el('dt', { text: '每轮存量部数' }), el('dd', { text: c.refresh_batch + (c.refresh_batch === 0 ? '（0 = 关闭存量轮转）' : '') }),
      el('dt', { text: '单轮预算' }), el('dd', { text: c.refresh_timeout_sec + ' 秒（超时后下一轮按游标续跑）' }),
      el('dt', { text: '出流签名密钥' }), el('dd', {}, flag(c.stream_secret_set, '已设置', '未设置')),
      el('dt', { text: 'WebDAV 凭据' }), el('dd', {},
        flag(c.webdav_user_set, '用户已设置', '用户未设置'), ' ',
        flag(c.webdav_pass_set, '密码已设置', '密码未设置')),
      el('dt', { text: 'Jellyfin API key' }), el('dd', {}, flag(j.api_key_set, '已设置', '未设置')));
  }

  function fillSettings(c) {
    baseline = {};
    for (const [key, id] of Object.entries(fields)) {
      baseline[key] = String(c[key]);
      document.getElementById(id).value = baseline[key];
    }
    setCredentialPlaceholders(c);
  }

  function setCredentialPlaceholders(c) {
    document.getElementById('set-webdav-user').value = '';
    document.getElementById('set-webdav-user').placeholder =
      c.webdav_user_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
    document.getElementById('set-webdav-pass').value = '';
    document.getElementById('set-webdav-pass').placeholder =
      c.webdav_pass_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
  }

  function fillJellyfin(j) {
    jellyfinBaseline = {};
    for (const f of jellyfinFields) {
      jellyfinBaseline[f.key] = String(j[f.view]);
      document.getElementById(f.id).value = jellyfinBaseline[f.key];
    }
    setJellyfinKeyPlaceholder(j);
  }

  function setJellyfinKeyPlaceholder(j) {
    const input = document.getElementById('set-jellyfin-api-key');
    input.value = '';
    input.placeholder = j.api_key_set ? '已设置，留空不改' : '未设置（这一段不启用）';
  }

  function updateHint() {
    if (!adminWritable) {
      saveBtn.disabled = true;
      hint.textContent = '未设置 DRAMA_HUB_ADMIN_TOKEN：后台改配置不可用（所有写操作都会 401）。' +
        '在服务端加上这个 env 再重启，然后用这个表单改配置。';
      return;
    }
    saveBtn.disabled = adminToken() === '';
    hint.textContent = adminToken() === ''
      ? '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再保存（令牌只存在本标签页）。'
      : '只提交改动过的项；密码与 API key 不回显，留空表示不改。';
  }

  async function load() {
    let status;
    try {
      status = await api('GET', '/api/status');
    } catch (err) {
      fail(err);
      return;
    }
    adminWritable = status.admin_token_set === true;
    if (!prefilled) {
      prefilled = true;
      fillSettings(status.config);
      fillJellyfin(status.jellyfin);
    } else {
      // 凭据输入框每次回读都清空：值不回显，占位符按最新的「已设置/未设置」更新。
      setCredentialPlaceholders(status.config);
      setJellyfinKeyPlaceholder(status.jellyfin);
    }
    renderReadonly(status.config, status.jellyfin);
    updateHint();
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const payload = {};
    for (const [key, id] of Object.entries(fields)) {
      const value = document.getElementById(id).value.trim();
      if (value !== baseline[key]) payload[key] = value;
    }
    for (const [key, id] of Object.entries(credentials)) {
      const value = document.getElementById(id).value.trim();
      if (value !== '') payload[key] = value;
    }
    for (const f of jellyfinFields) {
      // 前缀里的尾随空格有意义（`短剧 · `），这里不 trim，否则每次保存都会被当成改动重发。
      const value = document.getElementById(f.id).value;
      if (value !== (jellyfinBaseline[f.key] || '')) payload[f.key] = value;
    }
    // key 只写不读：留空 = 不改（要清空就显式传空串，这条只走 API）。
    const jellyfinKey = document.getElementById('set-jellyfin-api-key').value.trim();
    if (jellyfinKey !== '') payload.jellyfin_api_key = jellyfinKey;
    if (Object.keys(payload).length === 0) {
      say('没有改动', 'ok');
      return;
    }
    saveBtn.disabled = true;
    say('正在写入配置 …', 'busy');
    try {
      const data = await api('PATCH', '/api/settings', { settings: payload });
      // 回显归一化后的值（例：strm 前缀的尾 `/` 已被去掉），并把基线更新成刚写进去的值。
      for (const [key, value] of Object.entries(data.settings)) {
        const id = fields[key];
        if (!id || value === null) continue;
        document.getElementById(id).value = String(value);
        baseline[key] = String(value);
      }
      for (const f of jellyfinFields) {
        const value = data.settings[f.key];
        if (value === undefined || value === null) continue;
        document.getElementById(f.id).value = String(value);
        jellyfinBaseline[f.key] = String(value);
      }
      for (const id of Object.values(credentials)) document.getElementById(id).value = '';
      document.getElementById('set-jellyfin-api-key').value = '';
      say('已写入：' + Object.keys(data.settings).join('、') + '（生效时机见各字段徽标）', 'ok');
      await load();
    } catch (err) {
      fail(err);
    } finally {
      updateHint();
    }
  });

  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', updateHint);

  load();
}

// ---- 分派 ----

initAdminToken();
if (document.body.dataset.page === 'overview') initOverview();
if (document.body.dataset.page === 'settings') initSettings();
