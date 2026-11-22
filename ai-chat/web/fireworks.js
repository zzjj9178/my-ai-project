// ============================================================
//  中奖庆祝特效：全屏烟花 + 祝贺横幅
//
//  用法：celebrateLucky('幸运用户001')
//  特点：
//    · 无依赖，纯 canvas 粒子
//    · 没有 canvas 支持（或用户开了「减少动态效果」）时自动降级成只显示横幅
//    · 横幅不挡操作（3 秒后可点关闭，8 秒自动消失）
// ============================================================
(function () {
  var canvas = null, ctx = null, raf = 0, running = false;
  var rockets = [], sparks = [], launchUntil = 0, lastLaunch = 0, banner = null;

  function reducedMotion() {
    try {
      return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    } catch (e) { return false; }
  }

  function ensureCanvas() {
    if (canvas) return !!ctx;
    canvas = document.createElement('canvas');
    canvas.className = 'fw-canvas';
    document.body.appendChild(canvas);
    try { ctx = canvas.getContext && canvas.getContext('2d'); } catch (e) { ctx = null; }
    if (!ctx) {                         // jsdom / 老浏览器：别留下一个空 canvas，只放横幅
      if (canvas.parentNode) canvas.parentNode.removeChild(canvas);
      canvas = null;
      return false;
    }
    sizeCanvas();
    window.addEventListener('resize', sizeCanvas);
    return true;
  }

  function sizeCanvas() {
    if (!canvas || !ctx) return;
    var dpr = Math.min(window.devicePixelRatio || 1, 2);
    var w = window.innerWidth || document.documentElement.clientWidth || 360;
    var h = window.innerHeight || document.documentElement.clientHeight || 640;
    canvas.width = Math.floor(w * dpr);
    canvas.height = Math.floor(h * dpr);
    canvas.style.width = w + 'px';
    canvas.style.height = h + 'px';
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }

  function launchRocket() {
    var w = window.innerWidth || 360, h = window.innerHeight || 640;
    rockets.push({
      x: w * (0.12 + Math.random() * 0.76),
      y: h + 8,
      vy: -(7 + Math.random() * 3),
      targetY: h * (0.10 + Math.random() * 0.38),
      hue: Math.floor(Math.random() * 360)
    });
  }

  function explode(x, y, hue) {
    var n = 46 + Math.floor(Math.random() * 36);
    for (var i = 0; i < n; i++) {
      var a = (Math.PI * 2 * i) / n + Math.random() * 0.12;
      var sp = 1.4 + Math.random() * 3.6;
      sparks.push({
        x: x, y: y,
        vx: Math.cos(a) * sp, vy: Math.sin(a) * sp,
        life: 1, hue: hue + (Math.random() * 44 - 22)
      });
    }
  }

  function frame(now) {
    if (!ctx) return;
    var w = window.innerWidth || 360, h = window.innerHeight || 640;
    // 半透明擦除 = 拖尾效果（destination-out 保证背景依然是透明的）
    ctx.globalCompositeOperation = 'destination-out';
    ctx.fillStyle = 'rgba(0,0,0,0.20)';
    ctx.fillRect(0, 0, w, h);
    ctx.globalCompositeOperation = 'lighter';

    if (now < launchUntil && now - lastLaunch > 380) {
      lastLaunch = now;
      launchRocket();
    }

    var i, r, p;
    for (i = rockets.length - 1; i >= 0; i--) {
      r = rockets[i];
      r.y += r.vy;
      r.vy += 0.07;
      ctx.fillStyle = 'hsl(' + r.hue + ',100%,72%)';
      ctx.beginPath(); ctx.arc(r.x, r.y, 2.2, 0, Math.PI * 2); ctx.fill();
      if (r.y <= r.targetY || r.vy >= 0) {
        explode(r.x, r.y, r.hue);
        rockets.splice(i, 1);
      }
    }

    for (i = sparks.length - 1; i >= 0; i--) {
      p = sparks[i];
      p.x += p.vx; p.y += p.vy;
      p.vy += 0.036;
      p.vx *= 0.986; p.vy *= 0.986;
      p.life -= 0.0115;
      if (p.life <= 0) { sparks.splice(i, 1); continue; }
      ctx.fillStyle = 'hsla(' + p.hue + ',100%,' + (56 + p.life * 18) + '%,' + p.life + ')';
      ctx.beginPath(); ctx.arc(p.x, p.y, 2.4 * p.life + 0.6, 0, Math.PI * 2); ctx.fill();
    }

    if (now < launchUntil || rockets.length || sparks.length) {
      raf = requestAnimationFrame(frame);
    } else {
      stop();
    }
  }

  function stop() {
    running = false;
    if (raf) { cancelAnimationFrame(raf); raf = 0; }
    if (canvas) {
      if (canvas.parentNode) canvas.parentNode.removeChild(canvas);
      canvas = null; ctx = null;
    }
    rockets.length = 0; sparks.length = 0;
  }

  function showBanner(code) {
    if (banner && banner.parentNode) banner.parentNode.removeChild(banner);
    banner = document.createElement('div');
    banner.className = 'fw-banner';
    banner.innerHTML =
      '<div class="fw-inner">' +
        '<div class="fw-emoji">🎉</div>' +
        '<div class="fw-title">恭喜你成为本期幸运观众！</div>' +
        '<div class="fw-code">' + escapeText(code || '') + '</div>' +
        '<div class="fw-tip">请在会后凭「个人中心」页面领取精美礼品一份</div>' +
        '<button class="fw-close" type="button">知道啦</button>' +
      '</div>';
    document.body.appendChild(banner);
    var close = function () {
      if (banner && banner.parentNode) banner.parentNode.removeChild(banner);
      banner = null;
    };
    banner.querySelector('.fw-close').addEventListener('click', close);
    setTimeout(close, 9000);
  }

  function escapeText(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // ---------- 对外入口 ----------
  window.celebrateLucky = function (code) {
    showBanner(code);
    if (reducedMotion()) return;              // 尊重「减少动态效果」
    if (running) return;
    if (!ensureCanvas()) return;              // 没有 canvas 就只留横幅
    running = true;
    launchUntil = (window.performance ? performance.now() : Date.now()) + 4200;
    lastLaunch = 0;
    raf = requestAnimationFrame(frame);
  };
})();
