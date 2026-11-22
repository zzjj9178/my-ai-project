// ============================================================
//  公共逻辑：后端探测 / 登录 / WebSocket 聊天室
//  登录页 login.html 与主页 home.html 都会引用
// ============================================================

let BASE = '';      // 探测到的后端地址
let me = null;      // 当前用户（公开字段）
let contact = {};   // 当前用户的私密字段：{ qq, phone }
let lucky = null;   // 中奖信息：{ code: '幸运用户001' }，没中奖就是 null
let token = '';     // 登录凭证
let ws = null;
let reconnectTimer = null;

const $ = (id) => document.getElementById(id);

// ---------- 本机身份标识 ----------
// 存在 localStorage 里、退出登录也不清：服务端靠它在 1 小时内把你认成同一个人。
// 这样「离开后再登录」「改了昵称」都不会被当成新用户，也不会重复触发 AI 欢迎语。
function clientId() {
  let id = localStorage.getItem('ai_client_id');
  if (!id) {
    id = 'c' + Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
    localStorage.setItem('ai_client_id', id);
  }
  return id;
}
// 品牌改名后，浏览器里存过的旧资料要一起升级 ——
// 否则老用户打开登录页，看到的还是改版前存的「长大XXX萌新 / 长安大学」。
const SCHOOL_DEFAULT = '西安工业大学';
const LEGACY_SCHOOLS = ['长安大学'];
const LEGACY_NICK_PREFIX = [[/^西安工大/, '西工大'], [/^长大/, '西工大']];

function migrateProfile(p) {
  if (!p || typeof p !== 'object') return p;
  let changed = false;
  if (LEGACY_SCHOOLS.indexOf(p.school) >= 0) { p.school = SCHOOL_DEFAULT; changed = true; }
  if (typeof p.name === 'string') {
    for (let i = 0; i < LEGACY_NICK_PREFIX.length; i++) {
      const re = LEGACY_NICK_PREFIX[i][0];
      if (re.test(p.name)) { p.name = p.name.replace(re, LEGACY_NICK_PREFIX[i][1]); changed = true; break; }
    }
  }
  // 就地写回，下次打开不用再迁一遍
  if (changed) { try { localStorage.setItem('ai_profile', JSON.stringify(p)); } catch (e) {} }
  return p;
}

function savedProfile() {
  let p = null;
  try { p = JSON.parse(localStorage.getItem('ai_profile') || 'null'); } catch (e) { return null; }
  return migrateProfile(p);
}
function rememberProfile(p) {
  try { localStorage.setItem('ai_profile', JSON.stringify(p)); } catch (e) {}
}

// ---------- 后端地址自动探测 ----------
async function detectBackend() {
  const host = location.hostname || '127.0.0.1';
  const isHttps = location.protocol === 'https:';
  const scheme = isHttps ? 'https://' : 'http://';
  const cands = [];

  // ① URL 参数强制指定：http://域名/?api=http://1.2.3.4:8082
  const forced = new URLSearchParams(location.search).get('api');
  if (forced) cands.push(forced.replace(/\/$/, ''));

  // ② 同源（单进程部署或 Nginx 反代）
  cands.push(location.origin);

  // ③ 常见端口扫一遍
  for (const p of [8082, 8081, 8080, 8000, 3000, 443]) {
    const u = (p === 443 ? 'https://' : scheme) + host + (p === 443 ? '' : ':' + p);
    if (u !== location.origin && !cands.includes(u)) cands.push(u);
  }

  for (const c of cands) {
    try {
      const r = await fetch(c + '/api/health', { cache: 'no-store', mode: 'cors' });
      if (!r.ok) continue;
      // 必须校验内容：很多 Web 服务器会把任意路径兜底返回 index.html 且状态 200
      const j = await r.json().catch(() => null);
      if (j && j.status === 'ok') { console.log('[后端]', c); return c; }
    } catch (e) { /* 换下一个 */ }
  }
  console.warn('[后端探测失败] 已尝试:', cands);
  return null;
}

async function ensureBackend() {
  if (BASE) return BASE;
  BASE = await detectBackend();
  return BASE;
}

// ---------- 工具 ----------
function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
function renderMarkdown(text) {
  const blocks = [];
  let out = escapeHtml(text);
  out = out.replace(/```(\w*)\n?([\s\S]*?)```/g, (_, lang, code) => {
    blocks.push('<pre><code>' + code.replace(/\n$/, '') + '</code></pre>');
    return ' BLOCK' + (blocks.length - 1) + ' ';
  });
  out = out.replace(/`([^`\n]+)`/g, '<code>$1</code>')
           .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>')
           .replace(/^[-*]\s+(.+)$/gm, '• $1')
           .replace(/\n/g, '<br>');
  return out.replace(/ BLOCK(\d+) /g, (_, i) => blocks[+i]);
}

// ---------- 登录 ----------
async function doLogin(onSuccess) {
  const name   = ($('in-name')   || {}).value?.trim() || '';
  const school = ($('in-school') || {}).value?.trim() || '';
  const major  = ($('in-major')  || {}).value?.trim() || '';
  const bio    = ($('in-bio')    || {}).value?.trim() || '';
  if (!name) {
    $('login-err').textContent = '请填写名称';
    return;
  }

  $('login-btn').disabled = true;
  $('login-err').textContent = '正在连接后端…';

  try {
    const base = await ensureBackend();
    if (!base) {
      throw new Error(location.protocol === 'https:'
        ? '连不上后端：https 页面无法直连 http 后端，需 Nginx/Caddy 反代 /api 与 /ws'
        : '连不上后端：请确认进程已启动、端口已放行');
    }
    const qqEl = $('in-qq'), phoneEl = $('in-phone');
    const payload = {
      name, school, major, bio,
      clientId: clientId(),
      qq: qqEl ? qqEl.value.trim() : '',
      phone: phoneEl ? phoneEl.value.trim() : ''
    };
    const resp = await fetch(base + '/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    });
    const data = await resp.json();
    if (!resp.ok || !data.ok) throw new Error(data.error || '登录失败');

    token = data.token;
    me = data.user;
    contact = data.contact || {};
    lucky = data.lucky || null;
    localStorage.setItem('ai_token', token);
    rememberProfile({ name, school, major, qq: payload.qq, phone: payload.phone });
    if (typeof onSuccess === 'function') onSuccess();
  } catch (e) {
    $('login-err').innerHTML = '⚠️ ' + escapeHtml(e.message) +
      '<br><span style="color:#9ca3af">尝试：' + escapeHtml((BASE || '?') + '/api/login') + '</span>';
    console.error('[登录失败]', BASE, e);
  } finally {
    $('login-btn').disabled = false;
  }
}

// 用已有 token 恢复登录态
async function tryRestore() {
  const saved = localStorage.getItem('ai_token');
  if (!saved) return false;
  try {
    const base = await ensureBackend();
    if (!base) return false;
    const r = await fetch(base + '/api/me?token=' + encodeURIComponent(saved));
    const d = await r.json();
    if (r.ok && d.ok) {
      token = d.token; me = d.user; contact = d.contact || {}; lucky = d.lucky || null;
      return true;
    }
  } catch (e) { /* 后端没起或 token 失效 */ }
  localStorage.removeItem('ai_token');
  return false;
}

async function doLogout() {
  try {
    await fetch((BASE || location.origin) + '/api/logout?token=' + encodeURIComponent(token));
  } catch (e) {}
  if (ws) { ws.onclose = null; ws.close(); }
  // 只清 token：clientId 和本地资料留着，下次进来还是同一个人（服务端也不会再欢迎一次）
  localStorage.removeItem('ai_token');
  location.href = 'login.html';
}

// 个人中心：保存资料。服务端不会因此调用 AI，所以不会重复触发欢迎语。
async function saveProfile(fields) {
  const base = await ensureBackend();
  if (!base) throw new Error('连不上后端');
  const r = await fetch(base + '/api/profile?token=' + encodeURIComponent(token), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(Object.assign({ clientId: clientId() }, fields))
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok || !d.ok) throw new Error(d.error || '保存失败');
  me = d.user;
  contact = d.contact || {};
  lucky = d.lucky || null;
  rememberProfile({
    name: fields.name, school: fields.school, major: fields.major,
    qq: fields.qq || '', phone: fields.phone || ''
  });
  return me;
}

// ---------- WebSocket ----------
function wsURL() {
  const scheme = BASE.startsWith('https') ? 'wss://' : 'ws://';
  return scheme + BASE.replace(/^https?:\/\//, '') + '/ws?token=' + encodeURIComponent(token);
}

function connectWS() {
  const bodyEl = $('chat-body');
  if (!bodyEl) return;                       // 登录页没有聊天室容器
  if (ws && ws.readyState === WebSocket.OPEN) return;
  try { ws = new WebSocket(wsURL()); } catch (e) { scheduleReconnect(); return; }

  ws.onopen = () => {
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
    addSys('已连接聊天室');
  };
  ws.onmessage = (ev) => {
    let m; try { m = JSON.parse(ev.data); } catch (e) { return; }
    handleWS(m);
  };
  ws.onclose = () => { addSys('连接断开，正在重连…'); scheduleReconnect(); };
  ws.onerror = () => {};
}

function scheduleReconnect() {
  if (reconnectTimer) return;
  reconnectTimer = setTimeout(() => { reconnectTimer = null; connectWS(); }, 3000);
}

function sendWS(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// ---------- 消息渲染 ----------
function scrollBottom() {
  const b = $('chat-body');
  if (b) b.scrollTop = b.scrollHeight;
}

function addSys(text) {
  const bodyEl = $('chat-body');
  if (!bodyEl) return;
  const d = document.createElement('div');
  d.className = 'sys';
  d.textContent = text;
  bodyEl.appendChild(d);
  scrollBottom();
}

// 时间格式化：今天只显示时分，跨天补上日期
function fmtTime(ts) {
  if (!ts) return '';
  const d = new Date(ts), now = new Date();
  const p = (n) => String(n).padStart(2, '0');
  const hm = p(d.getHours()) + ':' + p(d.getMinutes());
  const sameDay = d.getFullYear() === now.getFullYear()
    && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  return sameDay ? hm : (d.getMonth() + 1) + '/' + d.getDate() + ' ' + hm;
}

function addMessage(user, text, ts) {
  const bodyEl = $('chat-body');
  if (!bodyEl || !user) return;
  const isMe = me && user.id === me.id;

  const row = document.createElement('div');
  row.className = 'row' + (isMe ? ' me' : '');

  const av = document.createElement('div');
  av.className = 'mini-avatar';
  av.style.background = user.color || '#6366f1';
  av.textContent = user.initial || (user.name || '?').slice(0, 1);

  const wrap = document.createElement('div');
  wrap.className = 'msg-wrap';

  const line = document.createElement('div');
  line.className = 'who-line';
  line.textContent = (user.name || '?') + (ts ? ' · ' + fmtTime(ts) : '');

  const bubble = document.createElement('div');
  bubble.className = 'bubble' + (isMe ? ' me' : '');
  bubble.textContent = text;

  wrap.appendChild(line);
  wrap.appendChild(bubble);
  row.appendChild(av);
  row.appendChild(wrap);
  bodyEl.appendChild(row);
  scrollBottom();
}

// 渲染一条完整的 AI 消息（历史回放用；流式过程由 aiStart/aiDelta 负责）
function addAIMessage(text, asker, ts) {
  const bodyEl = $('chat-body');
  if (!bodyEl || !text) return;

  const row = document.createElement('div');
  row.className = 'row';

  const av = document.createElement('div');
  av.className = 'mini-avatar';
  av.style.background = '#8b5cf6';
  av.textContent = 'AI';

  const wrap = document.createElement('div');
  wrap.className = 'msg-wrap';

  const line = document.createElement('div');
  line.className = 'who-line';
  line.textContent = 'AI 助手'
    + (asker ? ' · 回答 ' + asker : '')
    + (ts ? ' · ' + fmtTime(ts) : '');

  const bubble = document.createElement('div');
  bubble.className = 'ai-bubble';
  bubble.innerHTML = renderMarkdown(text);

  wrap.appendChild(line);
  wrap.appendChild(bubble);
  row.appendChild(av);
  row.appendChild(wrap);
  bodyEl.appendChild(row);
  scrollBottom();
}

// AI 流式消息
const aiNodes = new Map();
function aiStart(msg) {
  const bodyEl = $('chat-body');
  if (!bodyEl) return;

  const row = document.createElement('div');
  row.className = 'row';

  const av = document.createElement('div');
  av.className = 'mini-avatar';
  av.style.background = '#8b5cf6';
  av.textContent = 'AI';

  const wrap = document.createElement('div');
  wrap.className = 'msg-wrap';

  const line = document.createElement('div');
  line.className = 'who-line';
  line.textContent = 'AI 助手'
    + (msg.asker ? ' · 回答 ' + msg.asker : '')
    + (msg.ts ? ' · ' + fmtTime(msg.ts) : '');

  const bubble = document.createElement('div');
  bubble.className = 'ai-bubble';
  bubble.innerHTML = '<span class="txt"></span><span class="cursor"></span>';

  wrap.appendChild(line);
  wrap.appendChild(bubble);
  row.appendChild(av);
  row.appendChild(wrap);
  bodyEl.appendChild(row);
  scrollBottom();

  aiNodes.set(msg.id, {
    txt: bubble.querySelector('.txt'),
    cursor: bubble.querySelector('.cursor'),
    line: line,
    asker: msg.asker || '',
    raw: ''
  });
}
function aiDelta(msg) {
  const n = aiNodes.get(msg.id);
  if (!n) return;
  n.raw += msg.delta;
  n.txt.innerHTML = renderMarkdown(n.raw);
  scrollBottom();
}
function aiEnd(msg) {
  const n = aiNodes.get(msg.id);
  // 没有活着的节点：说明是在我加入前就开始生成的（或中途刷新），
  // ai_end 里带着全文和提问者，直接补一条完整的 AI 消息
  if (!n) {
    addAIMessage(msg.text, msg.asker, msg.ts);
    return;
  }
  if (n.cursor) n.cursor.remove();
  // 用 ai_end 的全文兜底，防止个别 delta 丢失导致内容缺字
  if (msg.text && msg.text.length > n.raw.length) {
    n.txt.innerHTML = renderMarkdown(msg.text);
  }
  if (msg.ts) {
    n.line.textContent = 'AI 助手'
      + (n.asker ? ' · 回答 ' + n.asker : '')
      + ' · ' + fmtTime(msg.ts);
  }
  aiNodes.delete(msg.id);
}

function renderOnline(list) {
  const bar = $('online-bar');
  if (!bar) return;
  bar.innerHTML = '';

  const arr = (list || []).slice();
  // 自己永远排第一，其余按昵称排序。
  // 服务端是遍历 map 下发的，顺序本来每次都可能不一样，不排一下名单会一直跳。
  const rank = (u) => (me && u.id === me.id ? 0 : 1);
  arr.sort((a, b) => rank(a) - rank(b) ||
    String(a.name || '').localeCompare(String(b.name || ''), 'zh-Hans-CN'));

  arr.forEach(u => {
    const isMe = me && u.id === me.id;
    const s = document.createElement('span');
    s.className = 'who' + (isMe ? ' me-tag' : '');
    s.innerHTML =
      '<span class="dot" style="background:' + (u.color || '#6366f1') + '">' +
      escapeHtml(u.initial || (u.name || '?').slice(0, 1)) + '</span>' +
      escapeHtml(u.name) + (isMe ? '（我）' : '');
    bar.appendChild(s);
  });
  const num = $('online-num');
  if (num) num.textContent = arr.length;
}

function handleWS(msg) {
  switch (msg.type) {
    case 'init':
      renderOnline(msg.online);
      break;
    case 'history':
      (msg.messages || []).forEach(raw => {
        let o = raw;
        if (typeof raw === 'string') { try { o = JSON.parse(raw); } catch (e) { return; } }
        if (!o) return;
        if (o.type === 'msg' && o.user) addMessage(o.user, o.text, o.ts);
        else if (o.type === 'ai_end')   addAIMessage(o.text, o.asker, o.ts);  // 历史里的 AI 回答（含欢迎语）
        else if (o.type === 'join')  addSys((o.user && o.user.name) + ' 加入了聊天室');
        else if (o.type === 'leave') addSys((o.user && o.user.name) + ' 离开了聊天室');
      });
      break;
    case 'msg':   addMessage(msg.user, msg.text, msg.ts); break;
    // 大房间里 join/leave 不再内联全量在线名单（那是 O(N²) 流量），
    // 改由服务端合并成固定间隔的 online 事件下发；小房间仍然内联，两处都兼容
    case 'join':  addSys(msg.user.name + ' 加入了聊天室'); if (msg.online) renderOnline(msg.online); break;
    case 'leave': addSys(msg.user.name + ' 离开了聊天室'); if (msg.online) renderOnline(msg.online); break;
    case 'online': renderOnline(msg.online); break;
    // 被抽中成为幸运观众：服务端只推给本人
    case 'lucky':
      lucky = { code: msg.code, no: msg.no };
      addSys('🎁 恭喜你成为本期幸运观众：' + msg.code);
      if (typeof onLucky === 'function') onLucky(lucky);
      break;
    case 'ai_start': aiStart(msg); break;
    case 'ai_delta': aiDelta(msg); break;
    case 'ai_end':   aiEnd(msg);   break;
    case 'error':    addSys('⚠️ ' + msg.error); break;
  }
}

// ---------- 发送 ----------
function sendMessage(isAI) {
  const input = $('msg-input');
  if (!input) return;
  let text = input.value.trim();
  if (!text) return;
  if (isAI && !text.startsWith('@AI')) text = '@AI ' + text;
  sendWS({ type: 'msg', text });
  input.value = '';
}

// 页面加载后绑定聊天室事件（主页调用）
function bindChatUI() {
  const sendBtn = $('send-btn'), aiBtn = $('ai-btn'), input = $('msg-input');
  if (sendBtn) sendBtn.addEventListener('click', () => sendMessage(false));
  if (aiBtn)   aiBtn.addEventListener('click', () => sendMessage(true));
  if (input)   input.addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); sendMessage(false); }
  });
  const lo = $('logout-btn');
  if (lo) lo.addEventListener('click', doLogout);
}
