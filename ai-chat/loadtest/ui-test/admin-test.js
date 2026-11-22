// 管理后台 + 幸运观众（概率 + 人数上限，进房瞬间判定）端到端测试
// 用法：BASE=http://127.0.0.1:8086 node admin-test.js
const WebSocket = require('ws');
const BASE = process.env.BASE || 'http://127.0.0.1:8086';
let pass = 0, fail = 0;
const ok = (n, c, extra = '') => { c ? (pass++, console.log('  ✅ ' + n)) : (fail++, console.log('  ❌ ' + n + '   ' + extra)); };
const J = { 'Content-Type': 'application/json' };

const login = (name, clientId) => fetch(BASE + '/api/login', {
  method: 'POST', headers: J, body: JSON.stringify({ name, school: '西安工业大学', major: '计算机类', clientId })
}).then(r => r.json());

const adminLogin = (username, password) => fetch(BASE + '/api/admin/login', {
  method: 'POST', headers: J, body: JSON.stringify({ username, password })
}).then(async r => ({ status: r.status, body: await r.json().catch(() => ({})) }));

const adminApi = (path, token, body) => fetch(BASE + path + '?token=' + encodeURIComponent(token), {
  method: 'POST', headers: J, body: body ? JSON.stringify(body) : undefined
}).then(r => r.json());

const state = (token) => fetch(BASE + '/api/admin/state?token=' + encodeURIComponent(token)).then(r => r.json());

// 连上 WebSocket 收 ms 毫秒；记录 lucky 推送到达时刻，用来验证「进房瞬间判定」
function collectTimed(token, ms) {
  return new Promise(resolve => {
    const t0 = Date.now();
    const ws = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
    const msgs = []; let luckyAt = 0;
    ws.on('message', d => {
      let m; try { m = JSON.parse(d.toString()); } catch (e) { return; }
      if (m.type === 'lucky' && !luckyAt) luckyAt = Date.now();
      msgs.push(m);
    });
    ws.on('error', () => {});
    setTimeout(() => {
      try { ws.close(); } catch (e) {}
      resolve({ msgs, t0, delay: luckyAt ? luckyAt - t0 : -1 });
    }, ms);
  });
}

(async () => {
  console.log('【管理端登录】');
  ok('密码错误被拒绝', (await adminLogin('admin', 'wrong')).status === 401);
  ok('账号错误被拒绝', (await adminLogin('root', 'qwer1234')).status === 401);
  const good = await adminLogin('admin', 'qwer1234');
  ok('admin / qwer1234 登录成功', good.body.ok === true && !!good.body.token);
  const T = good.body.token;
  ok('未登录看不到控制台数据', !!(await fetch(BASE + '/api/admin/state').then(r => r.json())).error);

  console.log('\n【/admin 页面】');
  const html = await (await fetch(BASE + '/admin')).text();
  ok('/admin 能打开（不是首页）', /幸运观众控制台/.test(html));
  ok('页面有「中奖概率」设置', /cfg-prob/.test(html));
  ok('页面有「最多中奖人数」设置', /cfg-max/.test(html));
  ok('已移除「立即抽一位」和定时抽奖', !/api\/admin\/draw/.test(html) && !/cfg-draw/.test(html));

  const stamp = Date.now();
  const mk = (i) => login('概率用户' + i, 'cid-prob-' + stamp + '-' + i);

  console.log('\n【概率 100% + 上限 2：先进房的 2 位立刻中奖】');
  let st = await adminApi('/api/admin/config', T, { enabled: true, probability: 1.0, maxWinners: 2, reset: true });
  ok('设置已生效（概率 100%、上限 2、名单清空）',
    st.ok && st.enabled === true && st.probability === 1 && st.maxWinners === 2 && st.drawn === 0,
    JSON.stringify({ p: st.probability, max: st.maxWinners, drawn: st.drawn }));

  const u = [];
  for (let i = 1; i <= 5; i++) u.push(await mk(i));

  const r1 = await collectTimed(u[0].token, 6000);
  const r2 = await collectTimed(u[1].token, 6000);
  const r3 = await collectTimed(u[2].token, 4000);
  const r4 = await collectTimed(u[3].token, 4000);
  const r5 = await collectTimed(u[4].token, 4000);

  ok('第 1 位中奖', r1.msgs.some(m => m.type === 'lucky'));
  ok('第 2 位中奖', r2.msgs.some(m => m.type === 'lucky'));
  ok('第 3~5 位都没中奖（人数上限生效）',
    [r3, r4, r5].every(r => !r.msgs.some(m => m.type === 'lucky')));
  ok('中奖在进房瞬间就判定（<2 秒，不是定时器）',
    [r1, r2].every(r => r.delay >= 0 && r.delay < 2000), '延迟 ' + r1.delay + 'ms / ' + r2.delay + 'ms');

  const luckyEnds = (r) => r.msgs.filter(m => m.type === 'ai_end' && /恭喜你成为本期幸运观众/.test(m.text || ''));
  ok('中奖者收到了带「幸运观众」的欢迎语', luckyEnds(r1).length === 1 && luckyEnds(r2).length === 1);
  ok('欢迎语带编号与领奖提示',
    luckyEnds(r1).every(m => /幸运用户\d{3}/.test(m.text) && /个人中心/.test(m.text) && /精美礼品/.test(m.text)));
  ok('没中奖的人一条 AI 消息都没收到',
    [r3, r4, r5].every(r => !r.msgs.some(m => m.type === 'ai_end' || m.type === 'ai_start')));

  console.log('\n【个人中心：只有中奖者能看到编号】');
  for (let i = 1; i <= 5; i++) {
    const me = await fetch(BASE + '/api/me?token=' + u[i - 1].token).then(r => r.json());
    if (i <= 2) ok('第 ' + i + ' 位有 lucky.code', me.lucky && /幸运用户\d{3}/.test(me.lucky.code), JSON.stringify(me.lucky));
    else ok('第 ' + i + ' 位没有 lucky 字段', !me.lucky, JSON.stringify(me.lucky));
  }

  console.log('\n【管理端统计】');
  st = await state(T);
  ok('已中奖 2 / 上限 2', st.drawn === 2 && st.maxWinners === 2);
  ok('至少记录了这 5 位参与', st.joined >= 5, 'joined=' + st.joined);
  ok('实际中奖率 = 已中奖/参与人数',
    Math.abs(st.actualRate - (st.drawn / st.joined * 100)) < 0.01, st.actualRate + '%');
  ok('中奖名单 2 条且编号连续',
    st.winners.length === 2 && st.winners[0].code === '幸运用户001' && st.winners[1].code === '幸运用户002',
    JSON.stringify(st.winners.map(w => w.code)));

  console.log('\n【概率 0%：谁都不中奖】');
  await adminApi('/api/admin/config', T, { enabled: true, probability: 0, maxWinners: 100, reset: true });
  const z = [];
  for (let i = 6; i <= 8; i++) z.push(await mk(i));
  const zr = [];
  for (const x of z) zr.push(await collectTimed(x.token, 3000));
  ok('概率 0% 时没有任何人中奖', zr.every(r => !r.msgs.some(m => m.type === 'lucky')));
  st = await state(T);
  ok('管理端显示 0 人中奖', st.drawn === 0, 'drawn=' + st.drawn);

  console.log('\n【概率 100% + 上限 1：只中 1 个】');
  await adminApi('/api/admin/config', T, { enabled: true, probability: 1.0, maxWinners: 1, reset: true });
  const o = [];
  for (let i = 9; i <= 11; i++) o.push(await mk(i));
  const or = [];
  for (const x of o) or.push(await collectTimed(x.token, 5000));
  ok('只有 1 个人中奖', or.filter(r => r.msgs.some(m => m.type === 'lucky')).length === 1,
    or.map(r => (r.msgs.some(m => m.type === 'lucky') ? '中' : '未')).join(','));
  ok('中奖的是第一个进房的',
    or[0].msgs.some(m => m.type === 'lucky') && !or[1].msgs.some(m => m.type === 'lucky'));

  console.log('\n【防止刷概率：同一期反复进出只判定一次】');
  await adminApi('/api/admin/config', T, { enabled: true, probability: 0, maxWinners: 10, reset: true });
  const repeater = await mk(13);
  await collectTimed(repeater.token, 900);          // 第一次进房
  st = await state(T);
  const base = st.joined;
  for (let i = 0; i < 5; i++) await collectTimed(repeater.token, 700); // 再进出 5 次
  st = await state(T);
  ok('同一个人反复进出 5 次，参与人数不再增加', st.joined === base, base + ' -> ' + st.joined);

  console.log('\n【中奖者再次进入：不二次中奖、不二次欢迎】');
  await adminApi('/api/admin/config', T, { enabled: true, probability: 1.0, maxWinners: 2, reset: true });
  const winner = await mk(14);
  const w1 = await collectTimed(winner.token, 6000);
  ok('第一次进房就中奖', w1.msgs.some(m => m.type === 'lucky'));
  const code1 = (await fetch(BASE + '/api/me?token=' + winner.token).then(r => r.json())).lucky.code;
  st = await state(T);
  const drawnAfter = st.drawn, joinedAfter = st.joined;
  const again = [];
  for (let i = 0; i < 3; i++) again.push(await collectTimed(winner.token, 2500));
  ok('再次进出不会收到新的中奖推送', again.every(r => !r.msgs.some(m => m.type === 'lucky')));
  ok('中奖编号保持不变',
    (await fetch(BASE + '/api/me?token=' + winner.token).then(r => r.json())).lucky.code === code1);
  st = await state(T);
  ok('已中奖人数没有被重复计数', st.drawn === drawnAfter, drawnAfter + ' -> ' + st.drawn);
  ok('中奖者反复进出也不再增加参与人数', st.joined === joinedAfter, joinedAfter + ' -> ' + st.joined);

  console.log('\n【开始新一期后可以重新参与】');
  await adminApi('/api/admin/config', T, { enabled: true, probability: 1.0, maxWinners: 2, reset: true });
  st = await state(T);
  ok('新一期：中奖名单与已中奖数归零', st.drawn === 0 && st.winners.length === 0,
    JSON.stringify({ drawn: st.drawn, n: st.winners.length }));
  const w2 = await collectTimed(winner.token, 7000);
  ok('上一期的中奖者在「新一期」可以重新参与并中奖', w2.msgs.some(m => m.type === 'lucky'));
  st = await state(T);
  ok('新一期名单里就是他', st.winners.length === 1 && st.winners[0].userId === winner.user.id,
    JSON.stringify(st.winners.map(w => w.name)));

  console.log('\n【改名后中奖名单要跟着更新】');
  await adminApi('/api/admin/config', T, { enabled: true, probability: 1.0, maxWinners: 2, reset: true });
  const renamer = await mk(20);
  const rr = await collectTimed(renamer.token, 7000);
  ok('这位中奖了', rr.msgs.some(m => m.type === 'lucky'));
  st = await state(T);
  const before = st.winners.find(w => w.userId === renamer.user.id);
  ok('名单里显示的是进房时的昵称', !!before && before.name === renamer.user.name,
    before ? before.name : '(不在名单里)');

  const NEWNAME = '改名后的新昵称' + (stamp % 10000);
  const upd = await fetch(BASE + '/api/profile?token=' + renamer.token, {
    method: 'POST', headers: J,
    body: JSON.stringify({ name: NEWNAME, school: '西北工业大学', major: '软件工程' })
  }).then(r => r.json());
  ok('改名成功', upd.ok && upd.user.name === NEWNAME, JSON.stringify(upd.user && upd.user.name));

  st = await state(T);
  const after = st.winners.find(w => w.userId === renamer.user.id);
  ok('中奖名单里的昵称已跟着更新', !!after && after.name === NEWNAME,
    after ? after.name : '(不在名单里)');
  ok('学校也一起更新了', !!after && after.school === '西北工业大学', after ? after.school : '');
  ok('中奖编号没有变', !!after && !!before && after.code === before.code);

  console.log('\n【关闭策略：恢复「人人欢迎」】');
  await adminApi('/api/admin/config', T, { enabled: false, probability: 1.0, maxWinners: 1 });
  const normal = await mk(12);
  const nr = await collectTimed(normal.token, 9000);
  const nEnds = nr.msgs.filter(m => m.type === 'ai_end');
  ok('策略关闭后新用户会被正常欢迎', nEnds.length === 1, nEnds.length + ' 条');
  const nStart = nr.msgs.filter(m => m.type === 'ai_start').length;
  const nDelta = nr.msgs.filter(m => m.type === 'ai_delta').length;
  ok('欢迎语是流式的（有 ai_start，且至少推了 1 帧增量）', nStart === 1 && nDelta >= 1,
    'ai_start=' + nStart + ' ai_delta=' + nDelta);
  ok('增量是合并推送的（帧数远小于逐 token 的 ~30 帧）', nDelta <= 25,
    'ai_delta=' + nDelta);
  const streamText = nr.msgs.filter(m => m.type === 'ai_delta').map(m => m.delta).join('');
  ok('流式增量拼起来和 ai_end 的正文一致（前端最终会用全文兜底）',
    nEnds.length === 1 && streamText.length > 0 && nEnds[0].text.startsWith(streamText.slice(0, 10)),
    'delta 拼接=' + streamText.length + ' 字 / ai_end=' + (nEnds[0] ? nEnds[0].text.length : 0) + ' 字');
  ok('普通欢迎语里没有领奖文案', nEnds.every(m => !/幸运观众/.test(m.text || '')));
  ok('策略关闭后没有 lucky 推送', !nr.msgs.some(m => m.type === 'lucky'));

  console.log('\n结果：' + pass + ' 项通过，' + fail + ' 项失败');
  process.exit(fail ? 1 : 0);
})();
