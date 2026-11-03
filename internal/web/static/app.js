'use strict';
/* drama-hub 管理面脚本。
 *
 * 一套通用请求/渲染小工具 + 两个页面的 init，靠 body[data-page] 分派。
 * 只用原生 DOM API、不引框架、没有构建步骤（ADR-0002）；页面数据一律来自
 * /api/ 下的 JSON 端点，渲染结果与 API 返回一一对应。 */

// ---- 通用 ----

// ---- 管理令牌 ----
// 写操作（非 GET/HEAD/OPTIONS）要带 X-Admin-Token（env DRAMA_HUB_ADMIN_TOKEN）。
// 令牌只存在 sessionStorage：关掉标签页就没了 —— 改配置是低频动作，不值得长期留在浏览器。
const TOKEN_KEY = 'drama-hub-admin-token';
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

/** 请求管理 API 并返回解析后的 JSON（204 返回 null）。非 2xx 抛 Error（带服务端 message）。 */
async function api(method, path, body) {
  const init = { method, headers: { Accept: 'application/json' } };
  if (method !== 'GET') init.headers[TOKEN_HEADER] = adminToken();
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let payload = null;
  if (text !== '') {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }
  if (!res.ok) {
    const message = payload && payload.message ? payload.message : res.status + ' ' + res.statusText;
    const error = new Error(message);
    error.status = res.status; // 409 这类「不是故障的状态」页面要单独处理（见 syncJellyfin）
    throw error;
  }
  return payload;
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

/** 在页面的 #notice 上显示一条提示（busy / ok / error）。 */
function say(text, kind) {
  const box = document.getElementById('notice');
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

// ---- 剧目页 ----

function initDramas() {
  const form = document.getElementById('drama-form');
  const input = document.getElementById('series-id');
  const syncBtn = document.getElementById('sync-btn');
  const tbody = document.getElementById('dramas');
  const empty = document.getElementById('dramas-empty');

  function render(dramas) {
    tbody.replaceChildren();
    for (const drama of dramas) {
      const deleteBtn = el('button', {
        type: 'button', class: 'btn ghost', text: '删除',
        onclick: () => removeDrama(drama),
      });
      tbody.append(el('tr', {},
        el('td', {}, el('div', { class: 'title-cell' },
          el('span', { text: drama.title }),
          el('span', { class: 'sub', text: drama.provider + ':' + drama.provider_id }))),
        el('td', { class: 'num', text: drama.year === 0 ? '—' : String(drama.year) }),
        el('td', { class: 'num', text: drama.episodes + ' / ' + drama.episode_total }),
        el('td', { class: 'num', text: String(drama.emby_items) }),
        el('td', { class: 'mono', text: fmtTime(drama.sync_at) }),
        el('td', { class: 'num' }, deleteBtn)));
    }
    empty.hidden = dramas.length !== 0;
  }

  async function reload() {
    const data = await api('GET', '/api/dramas');
    render(data.dramas);
  }

  async function removeDrama(drama) {
    const ok = window.confirm('删除《' + drama.title + '》？它的 ' + drama.episodes +
      ' 集会一并删除。');
    if (!ok) return;
    try {
      await api('DELETE', '/api/dramas/' + drama.id);
      await reload();
      say('已删除《' + drama.title + '》', 'ok');
    } catch (err) {
      fail(err);
    }
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const seriesId = input.value.trim();
    if (seriesId === '') return;
    syncBtn.disabled = true;
    say('正在同步 ' + seriesId + ' …', 'busy');
    try {
      const data = await api('POST', '/api/dramas', { series_id: seriesId });
      render(data.dramas);
      input.value = '';
      say('同步完成，库里共 ' + data.dramas.length + ' 部剧', 'ok');
    } catch (err) {
      fail(err);
    } finally {
      syncBtn.disabled = false;
    }
  });

  reload().catch(fail);
}

// ---- 同步与缓存看板 ----

function initStatus() {
  const totalsBox = document.getElementById('totals');
  const sourcesBody = document.getElementById('sources');
  const configBody = document.getElementById('config');
  const form = document.getElementById('settings-form');
  const saveBtn = document.getElementById('settings-save');
  const hint = document.getElementById('settings-hint');
  const jellyfinBody = document.getElementById('jellyfin');
  const jellyfinBtn = document.getElementById('jellyfin-sync');
  const jellyfinHint = document.getElementById('jellyfin-hint');
  const jellyfinProgress = document.getElementById('jellyfin-progress');

  // 表单字段 ↔ settings 键。页面不另写一份校验：值原样交给 PATCH，越界由服务端的
  // 注册表拒（400），这里只显示服务端给的原因。
  const fields = {
    strm_prefix: 'set-strm-prefix',
    cache_max_mb: 'set-cache-max-mb',
    cache_lru_days: 'set-cache-lru-days',
    merge_chunk_size: 'set-merge-chunk-size',
    refresh_interval_sec: 'set-refresh-interval',
  };

  // WebDAV 凭据只写不读：/api/status 只报「是否已设置」，不回传值（凭据不该被多抄一份
  // 到浏览器），所以这两项没有基线，留空 = 不改。
  const credentials = {
    webdav_user: 'set-webdav-user',
    webdav_pass: 'set-webdav-pass',
  };

  // Jellyfin 的四项可改配置：输入框 id、settings 键、/api/status 里的字段名三者不同名，
  // 所以逐项列出来（不靠命名约定拼字符串）。它们不在 config 块里，基线单独存一份。
  const jellyfinFields = [
    { key: 'jellyfin_url', id: 'set-jellyfin-url', view: 'url' },
    { key: 'jellyfin_library_root', id: 'set-jellyfin-root', view: 'library_root' },
    { key: 'jellyfin_library_prefix', id: 'set-jellyfin-prefix', view: 'library_prefix' },
    { key: 'jellyfin_timeout_sec', id: 'set-jellyfin-timeout', view: 'timeout_sec' },
  ];
  let jellyfinBaseline = {};

  let baseline = {}; // 上次读到的值：提交时只发改过的项（清空 strm_prefix 也算改）
  let prefilled = false; // 表单只在首次加载与保存成功后回填，不冲掉正在打的值
  let adminWritable = false; // 服务端有没有配 DRAMA_HUB_ADMIN_TOKEN

  function stat(label, value, small) {
    return el('div', {},
      el('dt', { text: label }),
      el('dd', { class: small ? 'small' : null, text: value }));
  }

  function row(name, value, timing, note, mono) {
    return el('tr', {},
      el('td', { text: name }),
      el('td', { class: mono ? 'mono' : null }, value instanceof Node ? value : String(value)),
      el('td', { class: 'hint', text: timing }),
      el('td', { class: 'hint', text: note }));
  }

  function flag(set) {
    return el('span', { class: set ? 'tag' : 'tag warn', text: set ? '已设置' : '未设置' });
  }

  // sourceRow 渲染一个播放源：调度状态（上次运行/上次成功/下次/上次错误）来自
  // /api/status 的调度字段，说明与配置来自 settings 现状。
  function sourceRow(src) {
    const state = src.running
      ? el('span', { class: 'tag', text: '正在刷新' })
      : src.enabled
        ? el('span', { class: 'tag', text: '已启用' })
        : el('span', { class: 'tag warn', text: '已停用' });
    // base_url / request_timeout_sec 是启动时注入插件的构造期配置：显示正在生效的
    // 值，并明说 settings 现值是否还要重启才生效。
    const bootNote = 'base_url 生效值 ' +
      (src.base_url_effective === '' ? '插件内置默认' : src.base_url_effective) +
      '，请求超时生效值 ' + src.request_timeout_sec_effective + ' 秒（重启后生效），间隔 ' +
      src.interval_sec + ' 秒' +
      (src.restart_required
        ? '；settings 现值（base_url ' + (src.base_url === '' ? '空' : src.base_url) +
          ' / 超时 ' + src.request_timeout_sec + ' 秒）重启后生效'
        : '');
    const note = src.reason !== '' ? src.reason : bootNote;
    const error = src.last_error === ''
      ? '—'
      : el('span', { class: 'mono', text: src.last_error });
    return el('tr', {},
      el('td', { text: src.label === src.name ? src.name : src.name + '（' + src.label + '）' }),
      el('td', {}, state),
      el('td', { class: 'mono', text: fmtTime(src.last_run_at) }),
      el('td', { class: 'mono', text: fmtTime(src.last_ok_at) }),
      el('td', { class: 'mono', text: fmtTime(src.next_due_at) }),
      el('td', { class: 'hint' }, el('div', { text: note }), error),
      el('td', { class: 'num' },
        el('button', {
          type: 'button', class: 'btn ghost', text: '立即刷新',
          disabled: src.running || !src.enabled,
          onclick: (event) => refreshSource(src, event.currentTarget),
        })));
  }

  // refreshSource 打手动触发端点：202 = 一轮已派出去，409 = 该源正在跑，
  // 401 = 没带对管理令牌，404 = 没有这个源或已停用。结果都回读 /api/status
  // （不猜这一轮的结局）。
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

  // renderConfig 渲染「运行配置」表：每行都带生效时机（MKACG-617 的生效性矩阵）。
  function renderConfig(c) {
    configBody.replaceChildren(
      row('strm 前缀', c.strm_prefix === '' ? '空（按请求 Host 推导）' : c.strm_prefix, '即时',
        'strm 内 URL 前缀；空 = 用访问本服务的 Host 拼绝对地址，填了就用填的（写入时去掉尾 /）', true),
      row('缓存目录', c.cache_dir, '即时',
        'cache_dir，海报与分片落 <cache_dir>/{cover,seg}，写入中的临时文件在 tmp/', true),
      row('缓存上限', c.cache_max_mb + ' MB', '≤5 分钟',
        'cache_max_mb，0 = 不按大小淘汰；下一轮淘汰生效'),
      row('缓存保留', c.cache_lru_days + ' 天', '≤5 分钟',
        'cache_lru_days，超过 N 天未命中的条目删除'),
      row('合并分块集数', c.merge_chunk_size + ' 集 / 对外集', '下次重建',
        'merge_chunk_size，每 N 集并成 1 条对外剧集；改完下一次同步或首播回填重建派生表时生效，旧 id 随即 404'),
      row('刷新间隔', c.refresh_interval_sec + ' 秒', '≤60 秒',
        'refresh_interval_sec，一轮到期 = 上次运行 + 本值'),
      row('每轮存量部数', String(c.refresh_batch), '即时', 'refresh_batch，0 = 关闭存量轮转'),
      row('单轮预算', c.refresh_timeout_sec + ' 秒', '即时',
        'refresh_timeout_sec，超时后下一轮按游标续跑'),
      row('出流签名密钥', flag(c.stream_secret_set), '即时',
        'stream_secret（ADR-0003）；换了之后所有旧 URL 失效'),
      row('WebDAV 用户', flag(c.webdav_user_set), '即时', 'webdav_user'),
      row('WebDAV 密码', flag(c.webdav_pass_set), '即时', 'webdav_pass'));
  }

  // fillSettings 把库里现在的值填进表单，并记成基线（提交时只发改过的项）。
  function fillSettings(c) {
    baseline = {};
    for (const [key, id] of Object.entries(fields)) {
      baseline[key] = String(c[key]);
      document.getElementById(id).value = baseline[key];
    }
    setCredentialPlaceholders(c);
  }

  // setCredentialPlaceholders 把「已设置 / 未设置」写在凭据输入框的占位符里：
  // 值本身不回显（/api/status 只给 *_set 标志）。
  function setCredentialPlaceholders(c) {
    document.getElementById('set-webdav-user').value = '';
    document.getElementById('set-webdav-user').placeholder =
      c.webdav_user_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
    document.getElementById('set-webdav-pass').value = '';
    document.getElementById('set-webdav-pass').placeholder =
      c.webdav_pass_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
  }

  // syncHint 说明「现在能不能写」：没配 env 令牌就写不了；配了但要先在顶栏填上。
  function syncHint() {
    if (!adminWritable) {
      saveBtn.disabled = true;
      jellyfinBtn.disabled = true;
      hint.textContent = '未设置 DRAMA_HUB_ADMIN_TOKEN：后台改配置不可用（所有写操作都会 401）。' +
        '在服务端加上这个 env 再重启，然后用这个表单改配置。';
      jellyfinHint.textContent = '同步媒体库是写操作，同样需要 DRAMA_HUB_ADMIN_TOKEN。';
      return;
    }
    saveBtn.disabled = adminToken() === '';
    jellyfinBtn.disabled = adminToken() === '';
    jellyfinHint.textContent = adminToken() === ''
      ? '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再点「同步媒体库」（令牌只存在本标签页）。'
      : '只增不删：分类消失只在这里列成「孤儿库」。';
    hint.textContent = adminToken() === ''
      ? '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再保存（令牌只存在本标签页）。'
      : '写 PATCH /api/settings；非法值整批拒绝（400）且库里不动；密码不回显，留空表示不改。';
  }

  // renderJellyfin 渲染媒体库现状：配置四项 + 上次结果。孤儿库只报告，prune 不进 UI
  // （破坏性动作只走 API，少一个误点路径）。
  function renderJellyfin(j) {
    const orphans = j.orphans.length === 0 ? '—' : j.orphans.join('、');
    const conflicts = j.conflicts.length === 0 ? '—' : j.conflicts.join('、');
    const missing = j.missing.length === 0 ? '' : '（缺 ' + j.missing.join('、') + '）';
    const state = j.running
      ? el('span', { class: 'tag', text: '正在同步' })
      : j.configured
        ? el('span', { class: 'tag', text: '已启用' })
        : el('span', { class: 'tag warn', text: '未配置' });
    jellyfinBody.replaceChildren(
      row('状态', el('span', {}, state, missing), '即时',
        '地址 / API key / 媒体库根三项都非空才启用；留空 = 整段关闭'),
      row('地址', j.url === '' ? '—' : j.url, '即时', 'jellyfin_url', true),
      row('媒体库根', j.library_root === '' ? '—' : j.library_root, '即时',
        'jellyfin_library_root，必须是 Jellyfin 容器内看到的路径', true),
      row('库名前缀', j.library_prefix, '即时',
        'jellyfin_library_prefix，只操作带这个前缀的库'),
      row('API key', flag(j.api_key_set), '即时', 'jellyfin_api_key（只写不读，状态面不返回值）'),
      row('单次超时', j.timeout_sec + ' 秒', '即时', 'jellyfin_timeout_sec，每次出站请求一个超时'),
      row('上次同步', fmtTime(j.last_run_at) +
        (j.last_run_at === '' ? '' : '（耗时 ' + fmtDuration(j.duration_ms) + '）'), '—',
        '状态只在内存里：重启后回到「未运行」；刷新页面从 GET /api/jellyfin/libraries 读回'),
      row('上次成功', fmtTime(j.last_ok_at), '—', '孤儿与同名冲突不算失败，也会推进这一项'),
      row('上次错误', j.last_error === '' ? '—' : el('span', { class: 'mono', text: j.last_error }), '—',
        'Jellyfin 不可用只影响这一段：同步、WebDAV、播放都不受影响'),
      row('上次新建', String(j.created), '—', '这一轮建了几个库'),
      row('上次已存在', String(j.unchanged), '—',
        '分类已有自管库、这一轮一个字段都没动的条数（幂等命中）。本服务不改已存在的库，所以没有「更新」'),
      row('上次删除', String(j.removed), '—', 'prune 删掉的自管孤儿库数；默认不删，恒 0'),
      row('结果未知', String(j.uncertain), '—',
        '建库请求超时、Jellyfin 侧是否建成不确定的条数（不计入新建）。下一轮同步会以列库结果收口'),
      row('被拒分类', j.failed.length === 0 ? '—' : el('span', { class: 'mono', text: j.failed.join('、') }), '—',
        'Jellyfin 拒绝建库的分类：多半是库路径在 Jellyfin 容器内不存在。修好路径后下一轮自动补上'),
      row('孤儿库', orphans, '—',
        '分类已消失的自管库。默认不删，删除走 POST /api/jellyfin/libraries {"prune":true}'),
      row('冲突库', conflicts, '—', '同名库不是本服务建的：不建、不删、不接管'));
  }

  // jellyfinResultText 把「这一轮做了什么」写成一句话（MKACG-664 AC-2）。
  // 「更新」恒 0：已存在的库一律不改（不重命名、不改路径，幂等），写出来并带上解释，
  // 免得人以为是「更新数没显示」。last_error 已包含被拒分类与结果未知的细节，直接接上。
  function jellyfinResultText(j) {
    let text = '新建 ' + j.created + '、更新 0（已存在的库不改动）、已存在 ' + j.unchanged +
      '、删除 ' + j.removed + '、失败 ' + j.failed.length +
      ' · 耗时 ' + fmtDuration(j.duration_ms);
    if (j.last_error !== '') text += ' · ' + j.last_error;
    return text;
  }

  // renderJellyfinProgress 画「正在跑」或「上次同步：时间 + 结果」那一行。
  // 两态共用一行：运行中给出阶段 / 已处理 / 开始时间 / 已用时间，收尾后给出本轮结果。
  // 内容全部来自状态面，所以刷新页面看到的仍是同一句话（不需要额外持久化）。
  function renderJellyfinProgress(j) {
    if (j.running) {
      const parts = ['运行中'];
      if (j.phase_label !== '') parts.push('阶段：' + j.phase_label);
      if (j.total > 0) parts.push(j.processed + ' / ' + j.total);
      if (j.started_at !== '') {
        parts.push('开始于 ' + fmtTime(j.started_at));
        parts.push('已用 ' + fmtDuration(Date.now() - Date.parse(j.started_at)));
      }
      jellyfinProgress.textContent = parts.join(' · ');
      jellyfinProgress.dataset.kind = 'busy';
      return;
    }
    jellyfinProgress.dataset.kind = j.last_error === '' ? 'ok' : 'error';
    jellyfinProgress.textContent = j.last_run_at === ''
      ? ''
      : '上次同步：' + fmtTime(j.last_run_at) + ' · ' + jellyfinResultText(j);
  }

  // JELLYFIN_POLL_MS 是轮询间隔：一轮建库是按个串行发请求，1 秒足够看出进度在动。
  // JELLYFIN_POLL_MAX_MS 是轮询上限（一轮最长 = 单次超时 × (库数 + 1)，30 分钟封顶）：
  // 到点停止自动刷新，别让一个已经失控的页面永远每秒钟打服务端。
  const JELLYFIN_POLL_MS = 1000;
  const JELLYFIN_POLL_MAX_MS = 30 * 60 * 1000;
  let jellyfinPolling = false;

  // followJellyfin 轮询 GET /api/jellyfin/libraries 直到这一轮收尾。
  // 轮询这个端点而不是 /api/status：后者每次都要全量统计 drama / episode 表。
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
  }

  // fillJellyfin 把媒体库配置填进表单并记基线；api key 永远留空（只写不读）。
  function fillJellyfin(j) {
    jellyfinBaseline = {};
    for (const f of jellyfinFields) {
      jellyfinBaseline[f.key] = String(j[f.view]);
      document.getElementById(f.id).value = jellyfinBaseline[f.key];
    }
    setJellyfinKeyPlaceholder(j);
  }

  // setJellyfinKeyPlaceholder 只更新 key 输入框的占位符与清空值：值不回显。
  function setJellyfinKeyPlaceholder(j) {
    const input = document.getElementById('set-jellyfin-api-key');
    input.value = '';
    input.placeholder = j.api_key_set ? '已设置，留空不改' : '未设置（这一段不启用）';
  }

  // syncJellyfin 手动触发一轮媒体库同步：202 = 已派出去（随后轮询进度），
  // 409 = 上一轮还在跑（同样接着显示那一轮的进度），400 = 三项没配齐（响应体点名缺哪个键）。
  // 结果一律回读状态面，不猜结局。
  async function syncJellyfin() {
    jellyfinBtn.disabled = true;
    say('已请求同步 Jellyfin 媒体库 …', 'busy');
    try {
      await api('POST', '/api/jellyfin/libraries', {});
      // 进度与结果都在下面那一行，notice 里不留一句立刻就会过时的话。
      say('', '');
    } catch (err) {
      // 409 不是故障：单飞锁挡住了重复派发。页面要说「已在运行」而不是报一个红色错误，
      // 然后照样把正在跑的那一轮显示出来（AC-4）。
      if (err.status === 409) say('Jellyfin 媒体库同步已在运行，接着显示这一轮的进度', 'busy');
      else fail(err);
    } finally {
      jellyfinBtn.disabled = false;
      await load().catch(fail);
      await followJellyfin();
    }
  }

  async function load() {
    const status = await api('GET', '/api/status');
    const t = status.totals;
    totalsBox.replaceChildren(
      stat('剧目', String(t.dramas)),
      stat('集', String(t.episodes)),
      stat('对外集', String(t.emby_items)),
      stat('最近同步', fmtTime(t.last_sync_at), true));
    renderConfig(status.config);
    renderJellyfin(status.jellyfin);
    renderJellyfinProgress(status.jellyfin);
    // 刷新 / 首次打开时这一轮可能还在跑：接着看，不必等用户再点一次。
    if (status.jellyfin.running) followJellyfin();
    sourcesBody.replaceChildren(...status.sources.map(sourceRow));
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
    syncHint();
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
      const value = document.getElementById(f.id).value.trim();
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
      say('已写入：' + Object.keys(data.settings).join('、') + '（生效时机见「运行配置」）', 'ok');
      await load();
    } catch (err) {
      fail(err);
    } finally {
      syncHint();
    }
  });

  jellyfinBtn.addEventListener('click', syncJellyfin);

  // 顶栏令牌改动后立刻更新提示与按钮状态。
  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', syncHint);

  load().catch(fail);
}

// ---- 分派 ----

initAdminToken();
const page = document.body.dataset.page;
if (page === 'dramas') initDramas();
else if (page === 'status') initStatus();
