#!/usr/bin/env node
// claim_helper.js — bailucode 每日额度一键领取（Turnstile 全自动）
//
// 依赖：Node 18+、puppeteer-core（npm i puppeteer-core）、Chrome/Chromium、Xvfb（无显示器时）
// 外层用 xvfb-run 包裹即可在服务器无屏环境运行（bailucode2api 的 browser solver 已自动包裹）。
//
// stdin:  {"email":"...","password":"...","origin":"https://bailucode.com","chromePath":"/usr/bin/google-chrome"}
// stdout: {"ok":true,"human_verified":true,
//          "results":[{"slot":"slot_a","claimed":true,"already":false},
//                     {"slot":"slot_b","claimed":true,"already":false}],
//          "stats":{...}}
//
// 运行要点（勿改）：
//   1. 保持浏览器原生 UA，不要 setUserAgent：指纹不一致会导致验证失败。
//   2. 需要真实显示环境：外层 xvfb-run 提供虚拟屏幕。
//   3. 领取端点 POST /api/api_keys {action:"claim_*_promo", turnstileToken}。
const puppeteer = require('puppeteer-core');

const sleep = (ms) => new Promise(r => setTimeout(r, ms));

async function main() {
  const input = await new Promise((resolve, reject) => {
    let buf = '';
    process.stdin.on('data', d => buf += d);
    process.stdin.on('end', () => { try { resolve(JSON.parse(buf)); } catch (e) { reject(e); } });
    process.stdin.on('error', reject);
  });
  const { email, password } = input;
  const origin = input.origin || 'https://bailucode.com';
  const chromePath = input.chromePath || '/usr/bin/google-chrome';

  const browser = await puppeteer.launch({
    executablePath: chromePath,
    headless: false, // headful；外层 xvfb-run 提供虚拟显示
    args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-blink-features=AutomationControlled',
      '--window-size=1366,900', '--lang=zh-CN', '--no-first-run', '--no-default-browser-check'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1366, height: 900 });
  await page.evaluateOnNewDocument(() => {
    Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
    window.__tsToken = ''; window.__tsErr = '';
    const hook = () => {
      if (window.turnstile && !window.turnstile.__hooked) {
        const r = window.turnstile.render.bind(window.turnstile);
        window.turnstile.render = (host, opts) => {
          const cb = opts && opts.callback, ec = opts && opts['error-callback'];
          if (opts) opts.callback = (t) => { window.__tsToken = t || ''; if (cb) cb(t); };
          if (opts) opts['error-callback'] = (e) => { window.__tsErr = String(e); if (ec) ec(e); };
          return r(host, opts);
        };
        window.turnstile.__hooked = true;
      }
    };
    setInterval(hook, 50);
  });

  const statsNow = () => page.evaluate(async () => {
    const r = await fetch('/api/api_keys', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'get_stats' }),
    });
    return (await r.json()).stats || {};
  });
  const doClaim = (action, token) => page.evaluate(async (act, tok) => {
    const r = await fetch('/api/api_keys', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: act, turnstileToken: tok }),
    });
    return await r.json();
  }, action, token);

  try {
    await page.goto(origin + '/auth/login?return_url=%2Fapi_platform%2F', { waitUntil: 'networkidle2', timeout: 60000 });
    await page.waitForSelector('#email', { timeout: 30000 });
    await page.type('#email', email, { delay: 60 });
    await page.click('#submitBtn');
    await page.waitForSelector('#password', { visible: true, timeout: 15000 });
    await page.type('#password', password, { delay: 60 });
    await sleep(500);
    await page.click('#submitBtn');
    await page.waitForFunction(() => location.pathname.startsWith('/api_platform'), { timeout: 30000 }).catch(() => {});
    await page.goto(origin + '/api_platform/', { waitUntil: 'networkidle2', timeout: 60000 });
    if (!(await page.evaluate(() => !!document.getElementById('anthropicPromoClaimBtn')))) {
      throw new Error('login failed: cannot reach api_platform (url=' + page.url() + ')');
    }

    const slots = [
      { slot: 'slot_a', btn: 'anthropicPromoClaimBtn', action: 'claim_anthropic_promo', key: 'anthropic_promo_claimed' },
      { slot: 'slot_b', btn: 'openclawPromoClaimBtn', action: 'claim_openclaw_promo', key: 'openclaw_promo_claimed' },
    ];
    const results = [];
    for (const s of slots) {
      const st = await statsNow();
      if (st[s.key]) { results.push({ slot: s.slot, claimed: false, already: true }); continue; }

      // 打开领取弹窗 → 等 Turnstile token（managed 自动过，最长 45s）
      await page.evaluate((id) => document.getElementById(id).click(), s.btn);
      await sleep(1500);
      let token = '';
      for (let i = 0; i < 45; i++) {
        token = await page.evaluate(() => window.__tsToken || '');
        if (token) break;
        if (i === 18) { // 兜底：20s 后尝试点击挑战框
          for (const f of page.frames()) {
            if (f.url().includes('challenges.cloudflare.com')) {
              try { await f.click('body', { offset: { x: 32, y: 32 } }); } catch (_) {}
            }
          }
        }
        await sleep(1000);
      }
      const resp = await doClaim(s.action, token);
      const after = await statsNow();
      results.push({
        slot: s.slot,
        claimed: !!(resp.success || after[s.key]),
        already: !!resp.already_claimed,
        error: resp.success ? '' : (resp.message || ''),
      });
      // 关弹窗，准备下一个 slot
      await page.evaluate((slot) => {
        const m = document.getElementById(slot === 'slot_a' ? 'anthropicClaimModal' : 'openclawClaimModal');
        if (m) m.style.display = 'none';
      }, s.slot);
      await sleep(600);
    }
    const st = await statsNow();
    process.stdout.write(JSON.stringify({
      ok: results.every(r => r.claimed || r.already),
      human_verified: !!st.promo_human_verified,
      results, stats: st,
    }));
  } catch (e) {
    process.stdout.write(JSON.stringify({ ok: false, error: String(e && e.message || e), results: [] }));
  } finally {
    await browser.close().catch(() => {});
  }
}
main().catch(e => { process.stdout.write(JSON.stringify({ ok: false, error: String(e), results: [] })); process.exit(1); });
