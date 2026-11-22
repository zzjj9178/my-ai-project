// 前端改动验收：用 jsdom 真跑一遍 login.html / home.html 的脚本
// 运行：node uitest.js
const http = require('http');
const fs = require('fs');
const path = require('path');
const { JSDOM, VirtualConsole } = require('jsdom');

const ROOT = path.resolve(__dirname, '..', '..', 'web');
const REPO = path.resolve(__dirname, '..', '..');   // 仓库根目录（backend/ 在这下面）
const PORT = 8099;
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'application/javascript; charset=utf-8', '.png': 'image/png', '.svg': 'image/svg+xml' };

let passed = 0, failed = 0;
function ok(name, cond, extra = '') {
  if (cond) { passed++; console.log(`  ✅ ${name}`); }
  else { failed++; console.log(`  ❌ ${name} ${extra}`); }
}

// ---------- 静态服务器 ----------
const server = http.createServer((req, res) => {
  const p = path.join(ROOT, decodeURIComponent(req.url.split('?')[0]));
  if (!p.startsWith(ROOT) || !fs.existsSync(p) || fs.statSync(p).isDirectory()) { res.writeHead(404); return res.end('404'); }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(p)] || 'application/octet-stream' });
  fs.createReadStream(p).pipe(res);
});

// ---------- HTML 标签配平检查（结构错误靠肉眼很难发现） ----------
function unbalancedTags(html) {
  const VOID = new Set(['area','base','br','col','embed','hr','img','input','link','meta','param','source','track','wbr']);
  const body = html.replace(/<script[\s\S]*?<\/script>/gi, '').replace(/<style[\s\S]*?<\/style>/gi, '')
                   .replace(/<!--[\s\S]*?-->/g, '');
  const re = /<(\/?)([a-zA-Z][a-zA-Z0-9]*)\b[^>]*?(\/?)>/g;
  const stack = []; let m;
  while ((m = re.exec(body))) {
    const closing = m[1] === '/', tag = m[2].toLowerCase();
    if (VOID.has(tag) || m[3] === '/') continue;
    if (closing) {
      if (stack[stack.length - 1] === tag) stack.pop();
      else return `</${tag}> 对不上，栈顶是 <${stack[stack.length - 1] || '空'}>`;
    } else stack.push(tag);
  }
  return stack.length ? `未闭合：${stack.join(' > ')}` : null;
}

// ---------- 打开一个页面，注入 fetch / WebSocket 桩 ----------
function open(file, { token = null, me = null, storage = null, record = {} } = {}) {
  const vc = new VirtualConsole();           // 屏蔽 jsdom 的 "Not implemented: navigation" 噪音
  vc.on('jsdomError', () => {});
  const dom = new JSDOM(fs.readFileSync(path.join(ROOT, file), 'utf-8'), {
    url: `http://127.0.0.1:${PORT}/${file}`,
    runScripts: 'dangerously',
    resources: 'usable',
    pretendToBeVisual: true,
    virtualConsole: vc,
    beforeParse(window) {
      if (token) window.localStorage.setItem('ai_token', token);
      if (storage) Object.keys(storage).forEach(k => window.localStorage.setItem(k, storage[k]));
      window.fetch = async (url, opts = {}) => {
        const u = String(url);
        record.calls = record.calls || [];
        record.calls.push({ url: u, method: opts.method || 'GET', body: opts.body });
        if (u.includes('/api/health')) return json({ status: 'ok', online: 3, apikey_configured: true });
        if (u.includes('/api/me'))     return me ? json({ ok: true, user: me, token }) : json({ error: '未登录' }, 401);
        if (u.includes('/api/login'))  return json({ ok: true, token: 'tok-test', user: me || {}, contact: {} });
        if (u.includes('/api/profile')) return json({ ok: true, user: Object.assign({}, me, { name: '新名字' }), contact: { qq: '123456789', phone: '13800138000' } });
        // 管理后台：state 永远返回「概率 10%、关闭」，用来验证表单不会被自动刷新冲掉
        if (u.includes('/api/admin/login')) return json({ ok: true, token: 'ad-test', ttl: 43200 });
        if (u.includes('/api/admin/state')) return json({
          ok: true, enabled: true, probability: 0.83, maxWinners: 30, drawn: 2, joined: 3,
          actualRate: 66.7, online: 3, round: 7,
          winners: [
            { no: 1, code: '幸运用户001', userId: 'u1', name: '西工大运维萌新7228', school: '西安工业大学', at: '19:45:10' },
            { no: 2, code: '幸运用户002', userId: 'u2', name: '西工大运维萌新4169', school: '西安工业大学', at: '19:47:31' }
          ]
        });
        if (u.includes('/api/admin/config')) return json({ ok: true, enabled: true, probability: 0.5, maxWinners: 10, drawn: 0, joined: 0, actualRate: 0, winners: [], online: 0, round: 0 });
        return json({ error: 'not found' }, 404);
      };
      function json(obj, status = 200) {
        return { ok: status >= 200 && status < 300, status, json: async () => obj };
      }
      window.WebSocket = class {
        constructor(url) { record.wsURL = url; record.wsCount = (record.wsCount || 0) + 1; setTimeout(() => this.onopen && this.onopen(), 0); }
        send(d) { record.sent = (record.sent || []).push(d); }
        close() {}
      };
    },
  });
  return dom;
}

const sleep = (ms) => new Promise(r => setTimeout(r, ms));

(async () => {
  await new Promise(r => server.listen(PORT, '127.0.0.1', r));
  console.log(`静态服务器: http://127.0.0.1:${PORT}  (web/ 目录)\n`);

  console.log('【HTML 结构】');
  for (const f of ['home.html', 'login.html', 'index.html']) {
    const err = unbalancedTags(fs.readFileSync(path.join(ROOT, f), 'utf-8'));
    ok(`${f} 标签配平`, !err, err || '');
  }

  // ================= login.html =================
  console.log('【login.html】');
  {
    const rec = {};
    const dom = open('login.html', { record: rec });
    const w = dom.window, d = w.document;
    await sleep(400);
    const name = d.getElementById('in-name').value;
    const school = d.getElementById('in-school').value;
    ok('昵称已预填为随机「西工大…」昵称', /^西工大.+萌新\d{4}$/.test(name), `实际="${name}"`);
    ok('学校已预填「西安工业大学」', school === '西安工业大学', `实际="${school}"`);
    ok('存在醒目主按钮「一键免填进入聊天室」',
      /一键免填进入聊天室/.test(d.getElementById('login-btn').textContent));
    ok('主按钮是页面里最大的按钮（有 .big-btn 样式）',
      d.getElementById('login-btn').classList.contains('big-btn'));

    const before = name;
    d.getElementById('dice-btn').click();
    ok('🎲 能换一个昵称', d.getElementById('in-name').value !== before);

    // 改名字后再点主按钮，应该用改后的名字登录
    d.getElementById('in-name').value = '张同学';
    d.getElementById('in-major').value = '计算机类';
    d.getElementById('login-btn').click();
    await sleep(300);
    const loginCall = (rec.calls || []).find(c => c.url.includes('/api/login'));
    ok('点击后调用了 POST /api/login', !!loginCall && loginCall.method === 'POST');
    let payload = {};
    try { payload = JSON.parse(loginCall.body); } catch (e) {}
    ok('提交的是输入框里的昵称', payload.name === '张同学', JSON.stringify(payload));
    ok('提交的学校是西安工业大学', payload.school === '西安工业大学', JSON.stringify(payload));
    ok('登录成功后写入了 ai_token', w.localStorage.getItem('ai_token') === 'tok-test');
    ok('登录请求带上了 clientId（服务端据此识别同一个人）',
      !!payload.clientId && payload.clientId.length > 5, JSON.stringify(payload));
    ok('本机 clientId 已持久化', !!w.localStorage.getItem('ai_client_id'));
    ok('本地记住了资料（下次自动预填）', !!w.localStorage.getItem('ai_profile'));
    dom.window.close();
  }

  console.log('\n【login.html · 旧资料自动升级】');
  {
    // 模拟老用户：浏览器里存着改版前的资料
    const legacy = JSON.stringify({ name: '长大K8s萌新1234', school: '长安大学', major: '计算机类' });
    const dom = open('login.html', { storage: { ai_profile: legacy }, record: {} });
    await sleep(400);
    const d = dom.window.document, w = dom.window;
    ok('旧昵称「长大K8s萌新1234」自动升为「西工大K8s萌新1234」',
      d.getElementById('in-name').value === '西工大K8s萌新1234', d.getElementById('in-name').value);
    ok('旧学校「长安大学」自动升为「西安工业大学」',
      d.getElementById('in-school').value === '西安工业大学', d.getElementById('in-school').value);
    ok('专业等其它资料保留不变', d.getElementById('in-major').value === '计算机类');
    const back = JSON.parse(w.localStorage.getItem('ai_profile') || '{}');
    ok('升级结果已写回 localStorage（下次打开不用再迁）',
      back.name === '西工大K8s萌新1234' && back.school === '西安工业大学', JSON.stringify(back));
    dom.window.close();

    // 中间版本「西安工大…」也要收敛成「西工大…」
    const dom2 = open('login.html', {
      storage: { ai_profile: JSON.stringify({ name: '西安工大极客萌新9999', school: '西安工业大学' }) }, record: {}
    });
    await sleep(350);
    ok('中间版本「西安工大…」也收敛为「西工大…」',
      dom2.window.document.getElementById('in-name').value === '西工大极客萌新9999',
      dom2.window.document.getElementById('in-name').value);
    dom2.window.close();
  }

  console.log('\n【login.html · 手机端布局】');
  {
    const raw = fs.readFileSync(path.join(ROOT, 'login.html'), 'utf-8');
    // 先剥掉注释：注释里正好写着「而不是 align-items: center」，不去掉会误判
    const css = raw.replace(/\/\*[\s\S]*?\*\//g, '');
    const bodyBlock = (css.match(/body\s*\{([^}]*)\}/) || [, ''])[1];
    ok('竖排居中改用 auto 外边距（内容超高不再裁掉顶部 logo）',
      /\.login-box\s*\{[^}]*margin-top:\s*auto/.test(css) &&
      /align-items:\s*flex-start/.test(bodyBlock),
      bodyBlock.trim().slice(0, 40));
    ok('body 不再用 align-items: center 做垂直居中',
      !/align-items:\s*center/.test(bodyBlock), bodyBlock.trim().slice(0, 60));
    ok('body 用 min-height 而不是固定 height（内容高时能滚动）',
      /min-height:\s*100dvh/.test(bodyBlock) && !/html,\s*body\s*\{\s*height:\s*100%/.test(css));
    ok('留了刘海屏安全区',
      /safe-area-inset-top/.test(css) && /safe-area-inset-bottom/.test(css));
  }

  console.log('\n【login.html · 老用户再次进入】');
  {
    const rec = {};
    const dom = open('login.html', {
      storage: {
        ai_client_id: 'cid-returning',
        ai_profile: JSON.stringify({ name: '甲同学', school: '西安工业大学', major: '计算机类' })
      },
      record: rec
    });
    const w = dom.window, d = w.document;
    await sleep(400);
    ok('昵称预填上次的名字（不再随机）', d.getElementById('in-name').value === '甲同学',
      d.getElementById('in-name').value);
    ok('学校专业也预填了', d.getElementById('in-school').value === '西安工业大学' &&
      d.getElementById('in-major').value === '计算机类');
    ok('提示文案变成「欢迎回来」', /欢迎回来/.test(d.querySelector('p.sub').textContent));
    ok('按钮文案变成「继续进入聊天室」', /继续进入聊天室/.test(d.getElementById('login-btn').textContent));

    d.getElementById('login-btn').click();
    await sleep(300);
    const call = (rec.calls || []).find(c => c.url.includes('/api/login'));
    const body = JSON.parse(call.body);
    ok('复用同一个 clientId', body.clientId === 'cid-returning', body.clientId);
    dom.window.close();
  }

  // ================= home.html =================
  console.log('\n【home.html】');
  {
    const rec = {};
    const me = { id: 'u1', name: '西工大极客萌新8848', school: '西安工业大学', major: '计算机类', color: '#6366f1', initial: '西' };
    const dom = open('home.html', { token: 'tok-test', me, record: rec });
    const w = dom.window, d = w.document;
    await sleep(500);

    const tabs = [...d.querySelectorAll('.nav-tabs .tab-btn')];
    ok('顶部有 3 个标签页', tabs.length === 3, `实际 ${tabs.length} 个`);
    ok('标签文案与顺序符合要求', tabs.map(t => t.textContent.trim()).join('|') ===
      '💬 AI 互动大厅|🏛️ 俱乐部简介|☁️ 云原生架构',
      tabs.map(t => t.textContent.trim()).join('|'));
    ok('默认停在 AI 互动大厅', d.getElementById('tab-chat').classList.contains('active'));
    ok('默认聊天室占满全屏（body.chat-mode）', d.body.classList.contains('chat-mode'));
    ok('默认时其他 2 档是隐藏的',
      ['about', 'k8s'].every(n => !d.getElementById('tab-' + n).classList.contains('active')));
    ok('WebSocket 已建立且带上 token', /\/ws\?token=tok-test/.test(rec.wsURL || ''), rec.wsURL);
    ok('输入框和发送按钮已启用',
      !d.getElementById('msg-input').disabled && !d.getElementById('send-btn').disabled);

    // 结构防线：聊天室的每一块都必须在 .chat-panel 里面。
    // 曾经这里多写了一个 </div> 把面板提前闭合，消息区和输入框全跑到卡片外面，
    // 页面上看起来就是「消息和大厅分家了」。
    const panel = d.querySelector('#tab-chat .chat-panel');
    const inside = ['#online-bar', '#chat-body', '#pills', '.chat-input', '.hint'];
    ok('头部/在线列表/消息区/气泡/输入框/提示 都在同一个面板里',
      !!panel && inside.every(sel => panel.querySelector(sel)),
      inside.filter(sel => !panel.querySelector(sel)).join(', '));
    ok('消息区没有落在面板外面', !d.querySelector('#tab-chat > #chat-body'));
    ok('输入区没有落在面板外面', !d.querySelector('#tab-chat > .chat-input'));

    // 布局：Tab 在最顶层；大厅有自己的 logo；昵称不再单独占位
    const firstEl = d.body.firstElementChild;
    ok('Tab 栏是页面最顶层的元素', !!firstEl && firstEl.classList.contains('nav-tabs'),
      firstEl && firstEl.className);
    ok('已移除独立的顶部信息条（.topbar）', !d.querySelector('.topbar'));
    const chatHead = d.querySelector('#tab-chat .chat-head');
    ok('聊天大厅标题是「AI 互动大厅」',
      chatHead.querySelector('.brand-title').textContent.trim() === 'AI 互动大厅');
    const brandImg = chatHead.querySelector('.brand-logo img');
    ok('大厅图标换成了 DeepSeek 官方 logo',
      !!brandImg && /deepseek/i.test(brandImg.getAttribute('src')), brandImg && brandImg.getAttribute('src'));
    ok('DeepSeek 图标文件存在', !!brandImg && fs.existsSync(path.join(ROOT, brandImg.getAttribute('src'))));
    ok('大厅标题下面那句说明文字已删除', !chatHead.querySelector('.brand-sub'));

    // 标签栏和内容区必须对齐到同一条居中列，否则宽屏上会错位
    const tabsBox = d.querySelector('.nav-tabs .tabs');
    ok('标签栏与聊天面板用同一条 980px 居中列',
      w.getComputedStyle(tabsBox).maxWidth === '980px' &&
      w.getComputedStyle(chatHead.closest('.chat-panel')).maxWidth === '980px',
      `tabs=${w.getComputedStyle(tabsBox).maxWidth} panel=${w.getComputedStyle(chatHead.closest('.chat-panel')).maxWidth}`);
    ok('聊天头部不再显示个人昵称（信息交给在线列表）',
      !chatHead.querySelector('#p-name') && !chatHead.querySelector('.avatar'));

    // 「离开」按钮：放在聊天大厅里，文案不是「退出」
    const leave = chatHead.querySelector('#logout-btn');
    ok('「离开」按钮在聊天大厅头部', !!leave);
    ok('按钮文案是「离开」', leave && leave.textContent.trim() === '离开', leave && leave.textContent);
    ok('「离开」按钮样式醒目（不是小灰字）', !!leave && leave.classList.contains('leave-btn'));
    ok('顶部标签栏里不再有退出按钮', !d.querySelector('.nav-tabs #logout-btn'));

    // 在线人数挪到标题下面，且不挤标题
    const homeSrc = fs.readFileSync(path.join(ROOT, 'home.html'), 'utf-8');
    ok('在线人数挪到了「AI 互动大厅」标题下面',
      !!chatHead.querySelector('.brand-txt #online-toggle'));
    ok('标题与在线数在同一列里上下排列（不会横向挤压标题）',
      /\.chat-head \.brand-txt\s*\{[^}]*flex-direction:\s*column/.test(homeSrc));
    ok('标题禁止换行/省略，保证字体不被压缩',
      /\.chat-head \.brand-title\s*\{[^}]*white-space:\s*nowrap/.test(homeSrc));
    ok('在线人数字号比标题小',
      /\.online-chip\s*\{[^}]*font-size:\s*\.7em/.test(homeSrc) &&
      /\.chat-head \.brand-title\s*\{[^}]*font-size:\s*1\.05em/.test(homeSrc));

    // 「我的」按钮的图标
    const meBtn = d.getElementById('profile-btn');
    const meAv = d.getElementById('me-avatar');
    ok('「我的」按钮用本人头像色块做图标（不再是 emoji）',
      !!meAv && meAv.textContent === me.initial, meAv ? meAv.textContent : '没有头像元素');
    ok('头像色块用了本人配色（#6366f1）',
      !!meAv && /99,\s*102,\s*241/.test(meAv.style.background), meAv ? meAv.style.background : '');
    ok('「我的」按钮里不再出现 emoji', !/👤/.test(meBtn.textContent), meBtn.textContent);

    // 在线列表：默认展开、可折叠、自己排第一
    const chip = d.getElementById('online-toggle'), bar = d.getElementById('online-bar');
    ok('在线列表默认是展开的', !bar.hasAttribute('hidden'));
    ok('默认箭头是展开态（.open）', chip.classList.contains('open'));
    const others = [
      { id: 'u3', name: '西工大码力萌新6678', color: '#f97316', initial: '西' },
      { id: 'u2', name: '西工大Linux萌新1113', color: '#22c55e', initial: '西' },
    ];
    w.renderOnline(others.concat([me]));   // 故意把自己放在最后，验证前端会排到第一
    const names = [...bar.querySelectorAll('.who')].map(e => e.textContent);
    ok('在线人数统计正确', d.getElementById('online-num').textContent === '3',
      d.getElementById('online-num').textContent);
    ok('自己排在在线列表第一位', names[0].includes(me.name) && names[0].includes('（我）'), names.join(' / '));
    // 顺序不能跟着服务端下发顺序变：把同一批人反序再发一次，结果必须一致
    w.renderOnline(others.slice().reverse().concat([me]));
    const names2 = [...bar.querySelectorAll('.who')].map(e => e.textContent);
    ok('名单顺序稳定，不随服务端下发顺序（即加入顺序）变化',
      names.join('|') === names2.join('|'), `${names.join(' / ')}  VS  ${names2.join(' / ')}`);
    ok('其余成员按昵称排序', names[1] !== names[2], names.join(' / '));
    chip.click();
    ok('点一下可以折叠在线列表', bar.hasAttribute('hidden') && !chip.classList.contains('open'));
    chip.click();
    ok('再点一下重新展开', !bar.hasAttribute('hidden') && chip.classList.contains('open'));

    // 切换标签
    w.switchTab('k8s');
    ok('切到「云原生架构」后该档显示', d.getElementById('tab-k8s').classList.contains('active'));
    ok('切走后聊天档隐藏', !d.getElementById('tab-chat').classList.contains('active'));
    ok('切走后取消全屏锁定（内容可滚动）', !d.body.classList.contains('chat-mode'));
    const tabBtn = (n) => tabs.find(t => t.dataset.tab === n);
    ok('对应按钮高亮', tabBtn('k8s').classList.contains('active') && !tabBtn('chat').classList.contains('active'));

    // 俱乐部简介档：简介 + 荣誉 + 培养路线都在这里
    w.switchTab('about');
    const about = d.getElementById('tab-about');
    ok('俱乐部简介档能正常显示', about.classList.contains('active'));
    const aboutTxt = about.textContent;
    ok('简介档含「我们是谁」与共建单位',
      aboutTxt.includes('我们是谁') && aboutTxt.includes('CNCF') && aboutTxt.includes('Red Hat'));
    ok('共建方文案为「红帽公司及引领云科技公司」（不再是「授权培训伙伴」）',
      aboutTxt.includes('红帽公司及引领云科技公司') && !aboutTxt.includes('授权培训伙伴'));
    ok('共建方只写「西安工业大学」，不再带「信息工程学院」',
      aboutTxt.includes('西安工业大学') && !aboutTxt.includes('信息工程学院'));
    ok('简介档不含任何学长学姐姓名',
      !['许中坤', '王子恒', '全佳欣', '崔俊娜', '刘思阳', '谢忠杨'].some(n => aboutTxt.includes(n)));
    ok('简介档含跨院校分支说明（西安邮电大学、长安大学）',
      aboutTxt.includes('西安邮电') && aboutTxt.includes('长安大学') && !aboutTxt.includes('陕西科技大学'),
      '长安大学=' + aboutTxt.includes('长安大学') + ' 陕西科技大学=' + aboutTxt.includes('陕西科技大学'));
    ok('宣讲会日期已改为 2026.10.16',
      /2026 年秋季纳新宣讲会：2026\.10\.16/.test(aboutTxt),
      (aboutTxt.match(/宣讲会：[^<]*/) || ['(没找到)'])[0]);
    ok('宣讲会地点只写学校、不带校区',
      /2026\.10\.16 · 西安工业大学/.test(aboutTxt) && !/校区/.test(aboutTxt));
    ok('原「荣誉与培养路线」已合并进来',
      aboutTxt.includes('阶梯式成长图谱') && aboutTxt.includes('竞赛战绩') &&
      aboutTxt.includes('中国大学生计算机设计大赛'));
    ok('简介档含纳新流程与 AI-Chat 项目',
      aboutTxt.includes('纳新流程') && aboutTxt.includes('AI-Chat'));
    ok('旧的 roadmap 档已不存在', !d.getElementById('tab-roadmap'));
    ok('共建单位图标已渲染',
      !!about.querySelector('img[src$="icon-cncf.svg"]') && !!about.querySelector('img[src$="icon-redhat.svg"]'));

    // Linux 已合并进「云原生架构」，独立的 Linux 档不再存在
    w.switchTab('k8s');
    const k8s = d.getElementById('tab-k8s');
    const k8sTxt = k8s.textContent;
    ok('独立的 Linux 档已删除', !d.getElementById('tab-linux') && !tabBtn('linux'));
    ok('Linux 内容已合并到云原生架构档里',
      k8sTxt.includes('Linux —— 现代企业级 IT 工业') && k8sTxt.includes('Kubernetes —— 调度海量算力'));
    ok('合并后 Linux 排在 K8s 前面（先底座后编排）',
      k8sTxt.indexOf('Linux —— 现代企业级') < k8sTxt.indexOf('Kubernetes —— 调度海量算力'));

    // 版本号不再显示给用户
    ok('页面上不再显示版本号',
      !d.body.textContent.includes('v2026.09.26') && !d.querySelector('.ver'));
    ok('版本信息改为不可见的 meta 标记（便于排查）',
      !!d.querySelector('meta[name="build"]'));

    // Tab 样式：选中项紫色加粗 + 底部短下划线
    const css = fs.readFileSync(path.join(ROOT, 'home.html'), 'utf-8');
    ok('选中 Tab 为紫色加粗', /\.tab-btn\.active\s*\{[^}]*color:\s*#6d28d9[^}]*font-weight:\s*700/.test(css));
    ok('选中 Tab 有底部短紫色下划线', /\.tab-btn\.active::after\s*\{\s*width:\s*26px/.test(css));

    // 个人中心
    const pfBtn = d.getElementById('profile-btn'), modal = d.getElementById('profile-modal');
    ok('聊天大厅有「我的」入口', !!pfBtn);
    ok('个人中心弹窗默认关闭', modal.hasAttribute('hidden'));
    pfBtn.click();
    ok('点击「我的」打开个人中心', !modal.hasAttribute('hidden'));
    ok('个人中心只保留昵称/学校/专业三项',
      !!d.getElementById('pf-name') && !!d.getElementById('pf-school') && !!d.getElementById('pf-major') &&
      !d.getElementById('pf-phone') && !d.getElementById('pf-qq') && !d.getElementById('pf-bio'),
      ['pf-phone', 'pf-qq', 'pf-bio'].filter(id => d.getElementById(id)).join(',') + ' 仍存在');
    ok('点击个人中心不会自动聚焦输入框（手机不弹键盘）',
      d.activeElement !== d.getElementById('pf-name'),
      d.activeElement ? d.activeElement.tagName : 'null');
    ok('表单已用当前资料预填', d.getElementById('pf-name').value === me.name, d.getElementById('pf-name').value);
    d.getElementById('pf-name').value = '新名字';
    d.getElementById('pf-save').click();
    await sleep(300);
    const profCall = (rec.calls || []).find(c => c.url.includes('/api/profile'));
    ok('保存会调用 POST /api/profile', !!profCall && profCall.method === 'POST');
    const profBody = profCall ? JSON.parse(profCall.body) : {};
    ok('只提交昵称/学校/专业（不再带手机号/QQ/自我介绍）',
      profBody.name === '新名字' && profBody.school !== undefined && profBody.major !== undefined &&
      profBody.phone === undefined && profBody.qq === undefined && profBody.bio === undefined,
      JSON.stringify(profBody));
    ok('保存成功后弹窗自动关闭', modal.hasAttribute('hidden'));

    // 幸运观众：中奖卡片默认隐藏，中奖后出现
    ok('个人中心默认不显示中奖卡片', d.getElementById('pf-lucky').hasAttribute('hidden'));
    ok('「我的」按钮默认不显示 🎁', d.getElementById('me-gift').hasAttribute('hidden'));
    w.eval("lucky = { code: '幸运用户007' };");
    w.eval("window.onLucky()");
    ok('中奖后卡片显示编号',
      !d.getElementById('pf-lucky').hasAttribute('hidden') &&
      d.getElementById('pf-lucky-code').textContent === '幸运用户007',
      d.getElementById('pf-lucky-code').textContent);
    ok('中奖后「我的」按钮出现 🎁', !d.getElementById('me-gift').hasAttribute('hidden'));

    // 中奖烟花特效
    ok('首页引入了烟花脚本', /src="fireworks\.js"/.test(homeSrc));
    ok('烟花函数已注册', typeof w.celebrateLucky === 'function');
    w.handleWS({ type: 'lucky', code: '幸运用户009', no: 9 });
    await sleep(150);
    const fw = d.querySelector('.fw-banner');
    ok('收到中奖推送会弹出全屏庆祝横幅', !!fw);
    ok('横幅里有祝贺语与中奖编号',
      !!fw && /恭喜你成为本期幸运观众/.test(fw.textContent) && /幸运用户009/.test(fw.textContent),
      fw ? fw.textContent.trim().slice(0, 50) : '');
    ok('横幅里有领奖提示', !!fw && /精美礼品/.test(fw.textContent));
    ok('横幅按钮文案是「知道啦」', !!fw && /知道啦/.test(fw.textContent));
    if (fw) fw.querySelector('.fw-close').click();
    await sleep(80);
    ok('点「知道啦」后横幅消失', !d.querySelector('.fw-banner'));
    ok('没有 canvas 支持时不留空 canvas 元素（优雅降级）', !d.querySelector('.fw-canvas'));

    // 退出只清 token，保留 clientId（否则会被当成新用户）
    w.localStorage.setItem('ai_token', 'tok-test');
    w.doLogout();
    await sleep(200);
    ok('退出登录后仍保留 clientId', w.localStorage.getItem('ai_client_id') === 'cid-returning' ||
      !!w.localStorage.getItem('ai_client_id'));

    // 快捷提问气泡
    w.switchTab('chat');
    const pills = [...d.querySelectorAll('#pills .pill')];
    ok('输入框上方有 3 个快捷提问气泡（去向那个已按要求删除）', pills.length === 3, `实际 ${pills.length}`);
    ok('已删除「Linux/Go 降维打击」和「人形机器人」两个默认提问',
      !pills.some(p => /降维打击/.test(p.textContent)) && !pills.some(p => /人形机器人/.test(p.textContent)),
      pills.map(p => p.textContent.trim()).join(' / '));
    ok('不再有「学长学姐去向 / 内推」这个默认提问',
      !pills.some(p => /去了哪些公司|内推|学长学姐/.test(p.textContent)),
      pills.map(p => p.textContent.trim()).join(' / '));
    ok('保留「俱乐部简介 / 竞赛奖项 / 计算机设计大赛」三个提问',
      pills.some(p => /俱乐部.*(什么|加入)/.test(p.textContent)) &&
      pills.some(p => /奖项|竞赛/.test(p.textContent)) &&
      pills.some(p => /计算机设计大赛/.test(p.textContent)),
      pills.map(p => p.textContent.trim()).join(' / '));
    ok('气泡内容都是 @AI 提问', pills.every(p => p.dataset.q.startsWith('@AI ')));
    pills[1].click();
    ok('点气泡会自动填入输入框', d.getElementById('msg-input').value === pills[1].dataset.q,
      `实际="${d.getElementById('msg-input').value}"`);

    // 企业图标
    const imgs = [...d.querySelectorAll('.logo-tile img')];
    ok('页面仍保留企业图标（共建方等）', imgs.length >= 1, `实际 ${imgs.length}`);
    ok('图标都指向存在的 logos/ 文件', imgs.every(i => fs.existsSync(path.join(ROOT, i.getAttribute('src')))));

    // 文案里不应该再出现考试代码/分数线这类信息
    const text = d.body.textContent;
    const banned = ['EX200', 'CKA', 'RHCSA', 'RHCE', 'EX294', '210/300', '66%', '2.5', '通过标准', '考试时长', '有效期'];
    const hit = banned.filter(b => text.includes(b));
    ok('已剔除考试代码/分数线等敏感信息', hit.length === 0, `仍出现: ${hit.join(', ')}`);

    dom.window.close();
  }

  // ================= admin.html =================
  console.log('\n【admin.html】');
  {
    const rec = {};
    const dom = open('admin.html', { record: rec });
    const w = dom.window, d = w.document;
    await sleep(300);

    d.getElementById('ad-user').value = 'admin';
    d.getElementById('ad-pass').value = 'qwer1234';
    d.getElementById('ad-login').click();
    await sleep(400);
    ok('登录后进入控制台', !d.getElementById('panel-view').classList.contains('hidden'));
    ok('表单已按服务端值初始化（概率 83%）', d.getElementById('cfg-prob').value === '83',
      d.getElementById('cfg-prob').value);

    // ⚠️ 回归：中奖名单必须真的渲染出来（之前 refresh() 中途抛异常被 catch 吞掉，
    //    统计卡能更新、名单却永远显示「还没有人中奖」）
    ok('顶部摘要已更新（不再是「加载中…」）', /第\s*8\s*期/.test(d.getElementById('top-sub').textContent),
      d.getElementById('top-sub').textContent);
    const wrows = d.querySelectorAll('#winner-box tbody tr');
    ok('中奖名单渲染出 2 行', wrows.length === 2, '实际 ' + wrows.length + ' 行');
    ok('名单里带中奖编号与昵称',
      /幸运用户001/.test(d.getElementById('winner-box').textContent) &&
      /西工大运维萌新7228/.test(d.getElementById('winner-box').textContent),
      d.getElementById('winner-box').textContent.trim().slice(0, 80));
    ok('已中奖统计显示 2 / 30', d.getElementById('st-drawn').textContent === '2 / 30',
      d.getElementById('st-drawn').textContent);

    // 改一个值但不保存
    d.getElementById('cfg-prob').value = '50';
    d.getElementById('cfg-prob').dispatchEvent(new w.Event('input', { bubbles: true }));
    d.getElementById('cfg-max').value = '30';
    d.getElementById('cfg-max').dispatchEvent(new w.Event('input', { bubbles: true }));
    ok('出现「有未保存的修改」提示', !d.getElementById('dirty-tip').classList.contains('hidden'));
    ok('出现「撤销」按钮', !d.getElementById('cfg-revert').classList.contains('hidden'));

    // 等过一次自动刷新（3 秒一轮）
    await sleep(3600);
    ok('自动刷新不会把未保存的概率冲掉', d.getElementById('cfg-prob').value === '50',
      '实际 ' + d.getElementById('cfg-prob').value);
    ok('自动刷新不会把未保存的人数上限冲掉', d.getElementById('cfg-max').value === '30',
      '实际 ' + d.getElementById('cfg-max').value);
    ok('自动刷新仍在跑（统计区在更新）', d.getElementById('st-online').textContent === '3', d.getElementById('st-online').textContent);

    // 撤销
    d.getElementById('cfg-revert').click();
    await sleep(300);
    ok('点「撤销」回到服务端的值', d.getElementById('cfg-prob').value === '83', d.getElementById('cfg-prob').value);
    ok('撤销后提示消失', d.getElementById('dirty-tip').classList.contains('hidden'));

    // 改了再保存
    d.getElementById('cfg-prob').value = '30';
    d.getElementById('cfg-prob').dispatchEvent(new w.Event('input', { bubbles: true }));
    d.getElementById('cfg-save').click();
    await sleep(400);
    const cfgCall = (rec.calls || []).find(c => c.url.includes('/api/admin/config'));
    ok('保存会调用 /api/admin/config', !!cfgCall && cfgCall.method === 'POST');
    const body = cfgCall ? JSON.parse(cfgCall.body) : {};
    ok('提交的 probability 是 0.3（不是百分数）', body.probability === 0.3, JSON.stringify(body));
    ok('保存后清除了未保存提示', d.getElementById('dirty-tip').classList.contains('hidden'));
    dom.window.close();
  }

  // ================= 知识库内容约束 =================
  console.log('\n【知识库：不含真人姓名】');
  {
    const kb = fs.readFileSync(path.join(REPO, 'backend', 'knowledge.md'), 'utf-8');
    const names = ['许中坤', '王子恒', '全佳欣', '崔俊娜', '刘思阳', '谢忠杨', '郭镕恺', '罗倩倩', '周路远', '许云鹏'];
    const hit = names.filter(n => kb.includes(n));
    ok('知识库里没有任何学长学姐姓名', hit.length === 0, '仍在: ' + hit.join('、'));
    ok('宣讲会只写「主持人」，不写具体姓名',
      /宣讲会.*由主持人/.test(kb), (kb.match(/- \*\*宣讲会\*\*：[^\n]*/) || [''])[0]);
  }

  // ================= ICP 备案号（只放登录页） =================
  console.log('\n【ICP 备案信息】');
  {
    const ICP = '陕ICP备2026027446号-1';
    const MIIT = 'https://beian.miit.gov.cn/';
    const esc = MIIT.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    const re = new RegExp('<a[^>]*href="' + esc + '"[^>]*>' + ICP + '</a>');

    const login = fs.readFileSync(path.join(ROOT, 'login.html'), 'utf-8');
    ok('login.html 含备案号', login.includes(ICP));
    ok('备案号是可点击链接且指向 beian.miit.gov.cn（不是图片）', re.test(login));

    // 页面里确实渲染出可点的链接
    const dom = open('login.html', { record: {} });
    await sleep(300);
    const d = dom.window.document;
    const a = d.querySelector('a.icp-link');
    ok('登录页渲染出备案链接', !!a);
    ok('链接文字就是备案号', !!a && a.textContent.trim() === ICP);
    ok('新窗口打开且带 rel=noopener',
      !!a && a.getAttribute('target') === '_blank' && /noopener/.test(a.getAttribute('rel') || ''));
    dom.window.close();

    // 其他界面按要求不再展示（防止以后误加回来）
    for (const f of ['home.html', 'index.html']) {
      const src = fs.readFileSync(path.join(ROOT, f), 'utf-8');
      ok(`${f} 不再展示备案号`, !src.includes(ICP));
    }
  }

  // ================= 图标文件完整性 =================
  console.log('\n【静态资源】');
  {
    const logos = fs.readdirSync(path.join(ROOT, 'logos'));
    ok('logos/ 下有图标文件', logos.length >= 10, `${logos.length} 个`);
    ok('图标体积都很小（每个 < 20KB）',
      logos.every(f => fs.statSync(path.join(ROOT, 'logos', f)).size < 20480));
  }

  server.close();
  console.log(`\n结果：${passed} 项通过，${failed} 项失败`);
  process.exit(failed ? 1 : 0);
})();
