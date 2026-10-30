'use strict';
/* drama-hub 管理面脚本。
 *
 * 一套通用请求/渲染小工具 + 三个页面的 init，靠 body[data-page] 分派。
 * 只用原生 DOM API、不引框架、没有构建步骤（ADR-0002）；页面数据一律来自
 * /api/ 下的 JSON 端点，渲染结果与 API 返回一一对应。 */

// ---- 通用 ----

/** 请求管理 API 并返回解析后的 JSON（204 返回 null）。非 2xx 抛 Error（带服务端 message）。 */
async function api(method, path, body) {
  const init = { method, headers: { Accept: 'application/json' } };
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
      const playable = drama.playable_episodes === 0
        ? el('span', { class: 'tag warn', text: '0' })
        : String(drama.playable_episodes);
      tbody.append(el('tr', {},
        el('td', {}, el('div', { class: 'title-cell' },
          el('span', { text: drama.title }),
          el('span', { class: 'sub', text: drama.provider + ':' + drama.provider_id }))),
        el('td', { class: 'num', text: drama.year === 0 ? '—' : String(drama.year) }),
        el('td', { class: 'num', text: drama.episodes + ' / ' + drama.episode_total }),
        el('td', { class: 'num' }, playable),
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
  const cacheBody = document.getElementById('cache');
  const cacheNote = document.getElementById('cache-note');

  function stat(label, value, small) {
    return el('div', {},
      el('dt', { text: label }),
      el('dd', { class: small ? 'small' : null, text: value }));
  }

  function row(name, value, note, mono) {
    return el('tr', {},
      el('td', { text: name }),
      el('td', { class: mono ? 'mono' : null }, value instanceof Node ? value : String(value)),
      el('td', { class: 'hint', text: note }));
  }

  function flag(set) {
    return el('span', { class: set ? 'tag' : 'tag warn', text: set ? '已设置' : '未设置' });
  }

  api('GET', '/api/status').then((status) => {
    const t = status.totals;
    totalsBox.replaceChildren(
      stat('剧目', String(t.dramas)),
      stat('集', String(t.episodes)),
      stat('可播集', String(t.playable_episodes)),
      stat('对外集', String(t.emby_items)),
      stat('合并组', String(t.merge_groups)),
      stat('最近同步', fmtTime(t.last_sync_at), true));
    if (t.playable_episodes === 0 && t.episodes > 0) {
      say('库里还没有可用播放源：同步只保证剧与集齐全，episode.play_url 由出流按需解析。', 'busy');
    }

    const c = status.config;
    configBody.replaceChildren(
      row('strm 前缀', c.strm_prefix === '' ? '空（直连本服务）' : c.strm_prefix,
        'strm 内 URL 前缀，填缓存代理地址即经代理播放', true),
      row('分片目标时长', c.seg_target_sec + ' 秒', 'seg_target_sec，切点对齐关键帧'),
      row('预取分片数', c.seg_prefetch + ' 片', 'seg_prefetch'),
      row('pin 天数', c.pin_days + ' 天', 'pin_days，最近 N 天播过的合并集整集预取'),
      row('出流签名密钥', flag(c.stream_secret_set), 'stream_secret（ADR-0003）'),
      row('WebDAV 用户', flag(c.webdav_user_set), 'webdav_user'),
      row('WebDAV 密码', flag(c.webdav_pass_set), 'webdav_pass'));

    sourcesBody.replaceChildren(...status.sources.map((src) => row(
      src.label === src.name ? src.name : src.name + '（' + src.label + '）',
      src.enabled
        ? el('span', { class: 'tag', text: '已启用' })
        : el('span', { class: 'tag warn', text: '已停用' }),
      src.reason !== '' ? src.reason
        : 'base_url ' + (src.base_url === '' ? '用插件默认地址' : src.base_url) +
          '，请求超时 ' + src.request_timeout_sec + ' 秒')));

    const cache = status.cache;
    cacheBody.replaceChildren(
      row('模式', cache.mode === 'proxy' ? '经缓存代理' : '直连（无缓存）',
        cache.mode === 'proxy' ? '播放字节先到缓存代理' : 'strm_prefix 为空，播放不经过代理'),
      row('代理前缀', cache.prefix === '' ? '—' : cache.prefix, '缓存代理地址', true),
      row('建议容量', cache.max_size, '--vfs-cache-max-size 的建议值，不参与本服务运行逻辑'),
      row('建议保留', cache.max_age, '--vfs-cache-max-age 的建议值，不参与本服务运行逻辑'),
      row('rclone 参数', cache.flags, '缓存代理启动参数（--vfs-cache-mode full 起）', true));
    cacheNote.textContent = cache.note;
  }).catch(fail);
}

// ---- 分派 ----

const page = document.body.dataset.page;
if (page === 'dramas') initDramas();
else if (page === 'merges') initMerges();
else if (page === 'status') initStatus();
