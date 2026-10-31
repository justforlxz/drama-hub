'use strict';
/* drama-hub 管理面脚本。
 *
 * 一套通用请求/渲染小工具 + 三个页面的 init，靠 body[data-page] 分派。
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
    throw new Error(message);
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
        el('td', { class: 'num', text: String(drama.merge_groups) }),
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
      ' 集与 ' + drama.merge_groups + ' 个合并组会一并删除。');
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

// ---- 合并编排页 ----

function initMerges() {
  const select = document.getElementById('drama-select');
  const groupBody = document.getElementById('groups');
  const groupsEmpty = document.getElementById('groups-empty');
  const episodeBox = document.getElementById('episodes');
  const editorHint = document.getElementById('editor-hint');
  const selectionHint = document.getElementById('selection-hint');
  const labelInput = document.getElementById('group-label');
  const createBtn = document.getElementById('create-group');

  let episodes = [];
  let groups = [];
  let selected = []; // 按点击顺序排列的 episode id，下标 + 1 就是合并集时间轴上的 ord
  let episodeButtons = new Map(); // episode id → 按钮节点，换剧时重建

  function takenEpisodeIds() {
    const taken = new Map();
    for (const group of groups) {
      for (const member of group.members) taken.set(member.episode_id, group.label);
    }
    return taken;
  }

  function renderGroups() {
    groupBody.replaceChildren();
    for (const group of groups) {
      const order = group.members
        .map((member) => el('span', { class: 'tag', text: 'E' + member.ep_no }))
        .reduce((acc, chip) => acc.concat([chip, el('span', { class: 'mono', text: '→' })]), [])
        .slice(0, -1);
      groupBody.append(el('tr', {},
        el('td', { text: group.label }),
        el('td', {}, el('div', { class: 'members' }, order)),
        el('td', { class: 'num' }, el('button', {
          type: 'button', class: 'btn ghost', text: '删除',
          onclick: () => removeGroup(group),
        }))));
    }
    groupsEmpty.hidden = groups.length !== 0;
  }

  // buildEpisodeButtons 只在换剧时调用：按钮节点建一次，之后的选中/禁用状态靠
  // paintEpisodeState 就地改写。每次点选都重建节点会把 .episodes 的滚动位置弹回顶部
  // —— 一部剧有 76 集时，点了第 50 集还看得见它，靠的就是不重建。
  function buildEpisodeButtons() {
    episodeBox.replaceChildren();
    episodeButtons = new Map();
    for (const episode of episodes) {
      const button = el('button', {
        type: 'button',
        class: 'ep',
        text: 'E' + episode.ep_no,
        title: episode.title || episode.vid,
        'aria-label': '第 ' + episode.ep_no + ' 集',
      });
      button.addEventListener('click', () => toggle(episode.id));
      episodeButtons.set(episode.id, button);
      episodeBox.append(button);
    }
    paintEpisodeState(takenEpisodeIds());
  }

  // paintEpisodeState 就地刷新「是否已在合并组」与「选中顺序」两个状态。
  function paintEpisodeState(taken) {
    for (const episode of episodes) {
      const button = episodeButtons.get(episode.id);
      const ord = selected.indexOf(episode.id) + 1;
      const inGroup = taken.has(episode.id);
      button.disabled = inGroup;
      button.setAttribute('aria-pressed', ord > 0 ? 'true' : 'false');
      if (ord > 0) button.setAttribute('data-ord', String(ord));
      else button.removeAttribute('data-ord');
    }
    syncSelectionHint();
  }

  function syncSelectionHint() {
    createBtn.disabled = selected.length < 2;
    if (selected.length === 0) {
      selectionHint.textContent = '已选 0 集：按播放顺序点集，至少 2 集。';
      return;
    }
    const order = selected.map((id) => {
      const episode = episodes.find((item) => item.id === id);
      return 'E' + episode.ep_no;
    });
    selectionHint.textContent = '已选 ' + selected.length + ' 集：' + order.join(' → ');
  }

  function toggle(episodeId) {
    const index = selected.indexOf(episodeId);
    if (index >= 0) selected.splice(index, 1);
    else selected.push(episodeId);
    paintEpisodeState(takenEpisodeIds());
  }

  function autoLabel() {
    const numbers = selected
      .map((id) => episodes.find((item) => item.id === id).ep_no)
      .sort((a, b) => a - b);
    if (numbers.length === 0) return '';
    const from = numbers[0];
    const to = numbers[numbers.length - 1];
    return from === to ? '第 ' + from + ' 集' : '第 ' + from + '-' + to + ' 集';
  }

  async function reloadGroups() {
    const data = await api('GET', '/api/dramas/' + select.value + '/merge-groups');
    groups = data.groups;
    selected = selected.filter((id) => !takenEpisodeIds().has(id));
    renderGroups();
    paintEpisodeState(takenEpisodeIds());
  }

  async function removeGroup(group) {
    try {
      await api('DELETE', '/api/merge-groups/' + group.id);
      await reloadGroups();
      say('已删除合并组「' + group.label + '」（对外集号已重建）', 'ok');
    } catch (err) {
      fail(err);
    }
  }

  async function loadDrama() {
    const id = select.value;
    selected = [];
    labelInput.value = '';
    if (!id) {
      episodes = [];
      groups = [];
      renderGroups();
      buildEpisodeButtons();
      editorHint.textContent = '还没有剧目：先去剧目页同步一部。';
      return;
    }
    const [episodeData, groupData] = await Promise.all([
      api('GET', '/api/dramas/' + id + '/episodes'),
      api('GET', '/api/dramas/' + id + '/merge-groups'),
    ]);
    episodes = episodeData.episodes;
    groups = groupData.groups;
    editorHint.textContent = '已在合并组里的集不可再选（合并组之间不重叠）。';
    renderGroups();
    buildEpisodeButtons();
  }

  select.addEventListener('change', () => {
    loadDrama().catch(fail);
  });

  createBtn.addEventListener('click', async () => {
    const label = labelInput.value.trim() || autoLabel();
    createBtn.disabled = true;
    try {
      const data = await api('POST', '/api/dramas/' + select.value + '/merge-groups', {
        label,
        episode_ids: selected,
      });
      groups = data.groups;
      selected = [];
      labelInput.value = '';
      renderGroups();
      paintEpisodeState(takenEpisodeIds());
      say('已建合并组「' + label + '」（对外集号已重建）', 'ok');
    } catch (err) {
      createBtn.disabled = false;
      fail(err);
    }
  });

  (async () => {
    const data = await api('GET', '/api/dramas');
    select.replaceChildren();
    for (const drama of data.dramas) {
      select.append(el('option', {
        value: String(drama.id),
        text: drama.title + ' (' + drama.year + ') · ' + drama.episodes + ' 集',
      }));
    }
    await loadDrama();
  })().catch(fail);
}

// ---- 同步与缓存看板 ----

function initStatus() {
  const totalsBox = document.getElementById('totals');
  const sourcesBody = document.getElementById('sources');
  const configBody = document.getElementById('config');
  const form = document.getElementById('settings-form');
  const saveBtn = document.getElementById('settings-save');
  const hint = document.getElementById('settings-hint');

  // 表单字段 ↔ settings 键。页面不另写一份校验：值原样交给 PATCH，越界由服务端的
  // 注册表拒（400），这里只显示服务端给的原因。
  const fields = {
    strm_prefix: 'set-strm-prefix',
    cache_max_mb: 'set-cache-max-mb',
    cache_lru_days: 'set-cache-lru-days',
    refresh_interval_sec: 'set-refresh-interval',
  };

  // WebDAV 凭据只写不读：/api/status 只报「是否已设置」，不回传值（凭据不该被多抄一份
  // 到浏览器），所以这两项没有基线，留空 = 不改。
  const credentials = {
    webdav_user: 'set-webdav-user',
    webdav_pass: 'set-webdav-pass',
  };

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
      row('分片目标时长', c.seg_target_sec + ' 秒', '即时（仅未建索引的集）',
        'seg_target_sec，切点对齐关键帧；已建索引的 uid 不重算'),
      row('缓存目录', c.cache_dir, '即时',
        'cache_dir，海报/分片/索引落 <cache_dir>/{cover,seg,index,tmp}', true),
      row('缓存上限', c.cache_max_mb + ' MB', '≤5 分钟',
        'cache_max_mb，0 = 不按大小淘汰；下一轮淘汰生效'),
      row('缓存保留', c.cache_lru_days + ' 天', '≤5 分钟',
        'cache_lru_days，超过 N 天未命中的条目删除'),
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
      hint.textContent = '未设置 DRAMA_HUB_ADMIN_TOKEN：后台改配置不可用（所有写操作都会 401）。' +
        '在服务端加上这个 env 再重启，然后用这个表单改配置。';
      return;
    }
    saveBtn.disabled = adminToken() === '';
    hint.textContent = adminToken() === ''
      ? '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再保存（令牌只存在本标签页）。'
      : '写 PATCH /api/settings；非法值整批拒绝（400）且库里不动；密码不回显，留空表示不改。';
  }

  async function load() {
    const status = await api('GET', '/api/status');
    const t = status.totals;
    totalsBox.replaceChildren(
      stat('剧目', String(t.dramas)),
      stat('集', String(t.episodes)),
      stat('对外集', String(t.emby_items)),
      stat('合并组', String(t.merge_groups)),
      stat('最近同步', fmtTime(t.last_sync_at), true));
    renderConfig(status.config);
    sourcesBody.replaceChildren(...status.sources.map(sourceRow));
    adminWritable = status.admin_token_set === true;
    if (!prefilled) {
      prefilled = true;
      fillSettings(status.config);
    } else {
      // 凭据输入框每次回读都清空：值不回显，占位符按最新的「已设置/未设置」更新。
      setCredentialPlaceholders(status.config);
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
      for (const id of Object.values(credentials)) document.getElementById(id).value = '';
      say('已写入：' + Object.keys(data.settings).join('、') + '（生效时机见「运行配置」）', 'ok');
      await load();
    } catch (err) {
      fail(err);
    } finally {
      syncHint();
    }
  });

  // 顶栏令牌改动后立刻更新提示与保存按钮状态。
  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', syncHint);

  load().catch(fail);
}

// ---- 分派 ----

initAdminToken();
const page = document.body.dataset.page;
if (page === 'dramas') initDramas();
else if (page === 'merges') initMerges();
else if (page === 'status') initStatus();
