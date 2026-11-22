'use strict';
// sahand-mirror panel — vanilla JS, no external resources.
const $ = (s, el = document) => el.querySelector(s);
const main = $('#main');
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fa = n => Number(n).toLocaleString('fa-IR');
const when = t => (!t || t.startsWith('0001')) ? 'هرگز' : new Date(t).toLocaleString('fa-IR');
const size = n => { if (!n) return '—'; const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1024 && i < 4) { n /= 1024; i++; } return n.toFixed(i ? 1 : 0) + ' ' + u[i]; };
const ARCHS = [['amd64', 'amd64 (x86_64)'], ['arm64', 'arm64'], ['armv7', 'armv7'], ['386', '386'], ['other', 'سایر معماری‌ها (armv5/6, s390x, mips…)'], ['otheros', 'ویندوز/مک/BSD']];
let pollTimer = null;

function toast(msg) {
  const t = $('#toast'); t.textContent = msg; t.classList.remove('hidden');
  clearTimeout(t._h); t._h = setTimeout(() => t.classList.add('hidden'), 2600);
}

async function api(path, opts = {}) {
  const o = { method: opts.method || 'GET', headers: { 'X-Requested-With': 'sahand-mirror' }, credentials: 'same-origin' };
  if (opts.body !== undefined) { o.headers['Content-Type'] = 'application/json'; o.body = JSON.stringify(opts.body); }
  const r = await fetch('/api/' + path, o);
  if (r.status === 401 && path !== 'login') { showLogin(); throw new Error('login'); }
  if (opts.raw) { if (!r.ok) throw new Error((await r.json()).error || r.status); return r; }
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
  return j;
}

function copy(text) {
  const done = () => toast('کپی شد');
  if (navigator.clipboard && window.isSecureContext) { navigator.clipboard.writeText(text).then(done); return; }
  const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta); ta.select();
  try { document.execCommand('copy'); done(); } catch (e) { prompt('کپی کنید:', text); }
  ta.remove();
}

document.addEventListener('click', e => {
  const b = e.target.closest('[data-copy]');
  if (b) { copy(b.dataset.copy); }
});

function saveBlob(blob, name) {
  const a = document.createElement('a'); a.href = URL.createObjectURL(blob); a.download = name;
  document.body.appendChild(a); a.click(); setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
}

// ---------- login ----------
function showLogin() {
  clearInterval(pollTimer);
  $('#top').classList.add('hidden'); $('#banner').classList.add('hidden');
  main.innerHTML = `<form class="card login" id="lf"><h1>ورود به سهند میرور</h1>
    <label>نام کاربری</label><input name="u" value="admin" autocomplete="username" class="ltr">
    <label>رمز عبور</label><input name="p" type="password" autocomplete="current-password" class="ltr" autofocus>
    <div id="lerr"></div><p><button>ورود</button></p>
    <p class="muted">رمز را نصب‌کننده چاپ کرده است؛ برای رمز جدید روی سرور <code>sahand-mirror</code> را اجرا کنید (گزینهٔ ۳).</p></form>`;
  $('#lf').onsubmit = async e => {
    e.preventDefault();
    try { await api('login', { method: 'POST', body: { username: e.target.u.value, password: e.target.p.value } }); route(); }
    catch (err) { $('#lerr').innerHTML = `<div class="err">${esc(err.message)}</div>`; }
  };
}

// ---------- status banner ----------
async function refreshBanner() {
  const s = await api('status');
  const b = $('#banner'); b.classList.remove('hidden', 'ok', 'warn', 'run');
  const sy = s.sync;
  const lastOk = sy.last_ok && !sy.last_ok.startsWith('0001') ? new Date(sy.last_ok) : null;
  const lastTry = sy.last_attempt && !sy.last_attempt.startsWith('0001') ? new Date(sy.last_attempt) : null;
  if (sy.running) { b.classList.add('run'); b.textContent = '⏳ در حال همگام‌سازی… ' + (sy.progress || ''); }
  else if (lastTry && (!lastOk || lastOk < lastTry)) { b.classList.add('warn'); b.textContent = '⚠️ حالت آفلاین — آخرین همگام‌سازی موفق: ' + when(sy.last_ok) + (sy.last_import && !sy.last_import.startsWith('0001') ? ' · آخرین ورود بسته: ' + when(sy.last_import) : ''); }
  else if (!lastTry && sy.last_import && !sy.last_import.startsWith('0001')) { b.classList.add('warn'); b.textContent = '📦 حالت بسته‌ای — آخرین ورود بسته: ' + when(sy.last_import); }
  else if (!lastTry) { b.classList.add('warn'); b.textContent = 'هنوز همگام‌سازی نشده است'; }
  else if (sy.last_error) { b.classList.add('warn'); b.textContent = '✅ اتصال برقرار — ولی برخی برنامه‌ها خطا داشتند (' + when(sy.last_ok) + ')'; }
  else { b.classList.add('ok'); b.textContent = '✅ همگام با گیت‌هاب — آخرین همگام‌سازی موفق: ' + when(sy.last_ok); }
  return s;
}

function poll(fn) {
  clearInterval(pollTimer);
  pollTimer = setInterval(async () => { try { const s = await refreshBanner(); if (fn) fn(s); } catch (e) { } }, 3000);
}

// ---------- pages ----------
async function pageDashboard() {
  const s = await refreshBanner();
  main.innerHTML = `<h1>داشبورد</h1>
  <div class="grid">
    <div class="stat"><div class="k">آخرین همگام‌سازی موفق</div><div class="v">${esc(when(s.sync.last_ok))}</div></div>
    <div class="stat"><div class="k">برنامه‌ها / مخزن‌ها</div><div class="v">${fa(s.apps)} / ${fa(s.repos)}</div></div>
    <div class="stat"><div class="k">فایل‌های ریلیز</div><div class="v">${fa(s.assets)}</div></div>
    <div class="stat"><div class="k">فضای مصرفی / آزاد</div><div class="v ltr">${size(s.disk_used)} / ${size(s.disk_free)}</div></div>
  </div>
  <div class="card"><h2>آدرس هاب</h2>
    <div class="cmd"><span>${esc(s.hub)}</span><button data-copy="${esc(s.hub)}">کپی</button></div>
    <p class="muted">سرورهای داخلی و گوشی‌ها با این آدرس به هاب وصل می‌شوند. صفحهٔ دانلود عمومی: <a href="/d" target="_blank">/d</a> · فقط APK: <a href="/d?apk=1" target="_blank">/d?apk=1</a></p>
    <div class="row"><button id="syncAll" ${s.sync.running ? 'disabled' : ''}>همگام‌سازی همه</button>
    <span class="muted">${s.direct_sync ? 'همگام‌سازی خودکار روزانه ساعت ' + fa(s.sync_hour) : 'همگام‌سازی مستقیم خاموش است (حالت بسته‌ای)'}</span></div>
    ${s.sync.last_error ? `<h3>خطاهای آخرین همگام‌سازی</h3><div class="err ltr">${esc(s.sync.last_error)}</div>` : ''}
  </div>`;
  $('#syncAll').onclick = async () => { try { await api('sync', { method: 'POST' }); toast('همگام‌سازی شروع شد'); route(); } catch (e) { toast(e.message); } };
  let was = s.sync.running;
  poll(ns => { if (was && !ns.sync.running) route(); was = ns.sync.running; });
}

async function pageApps() {
  refreshBanner(); poll();
  const d = await api('apps');
  const rows = d.apps.map(a => {
    const rp = d.repos[(a.repos || [])[0]] || d.repos[(a.repo || '').toLowerCase()];
    const latest = rp ? (rp.latest || (rp.releases && rp.releases[0] && rp.releases[0].tag) || '—') : '—';
    const st = a.error ? '<span class="pill bad">خطا</span>' : (a.last_sync && !a.last_sync.startsWith('0001') ? '<span class="pill ok">آینه شده</span>' : '<span class="pill warn">همگام نشده</span>');
    return `<tr><td><a href="#/app/${esc(a.id)}"><b>${esc(a.name)}</b></a><div class="muted ltr">${esc(a.repo || '')}</div></td>
      <td>${a.kind === 'repo' ? 'مخزن' : 'دستور نصب'}</td><td class="ltr">${esc(latest)}</td><td>${fa((a.repos || []).length)}</td>
      <td>${st}</td><td class="muted">${esc(when(a.last_sync))}</td></tr>`;
  }).join('');
  main.innerHTML = `<h1>برنامه‌ها</h1>
  <div class="card"><h2>افزودن برنامه</h2>
    <div class="tabs"><button class="on" data-t="cmd">با دستور نصب</button><button data-t="repo">با مخزن گیت‌هاب</button></div>
    <form id="addf">
      <div id="t-cmd"><label>دستور نصب یک‌خطی</label><input name="command" class="ltr" placeholder="bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh)"></div>
      <div id="t-repo" class="hidden"><label>مخزن (owner/repo)</label><input name="repo" class="ltr" placeholder="sahand363/zimzim"></div>
      <label>نام (اختیاری)</label><input name="name">
      <p><button>افزودن و همگام‌سازی</button></p>
      <p class="muted">هاب اسکریپت نصب را می‌خواند، همهٔ مخزن‌های گیت‌هاب داخلش را پیدا می‌کند و N ریلیز آخر هرکدام را با همهٔ فایل‌ها آینه می‌کند. آدرس‌های غیرگیت‌هابی به‌عنوان «وابستگی خارجی» نمایش داده می‌شوند.</p>
    </form></div>
  <div class="card"><div class="row"><h2 class="grow">فهرست (${fa(d.apps.length)})</h2>
    <button id="syncAll" class="ghost">همگام‌سازی همه</button><a class="btn ghost" href="/api/apps.json" download>دانلود apps.json</a></div>
    <table><tr><th>نام</th><th>نوع</th><th>آخرین نسخه</th><th>مخزن‌ها</th><th>وضعیت</th><th>آخرین همگام‌سازی</th></tr>${rows}</table></div>`;
  let mode = 'cmd';
  main.querySelectorAll('.tabs button').forEach(b => b.onclick = e => {
    e.preventDefault(); mode = b.dataset.t;
    main.querySelectorAll('.tabs button').forEach(x => x.classList.toggle('on', x === b));
    $('#t-cmd').classList.toggle('hidden', mode !== 'cmd'); $('#t-repo').classList.toggle('hidden', mode !== 'repo');
  });
  $('#addf').onsubmit = async e => {
    e.preventDefault(); const f = e.target;
    const body = mode === 'cmd' ? { command: f.command.value, name: f.name.value } : { repo: f.repo.value, name: f.name.value };
    try { const a = await api('apps', { method: 'POST', body }); toast('اضافه شد'); location.hash = '#/app/' + a.id; } catch (err) { toast(err.message); }
  };
  $('#syncAll').onclick = async () => { try { await api('sync', { method: 'POST' }); toast('همگام‌سازی شروع شد'); } catch (e) { toast(e.message); } };
}

async function pageApp(id) {
  refreshBanner();
  const d = await api('apps/' + encodeURIComponent(id));
  const a = d.app, hub = d.hub;
  const primary = d.repos[(a.repo || '').toLowerCase()] || Object.values(d.repos)[0];
  const tags = primary ? (primary.releases || []).map(r => r.tag) : [];
  const kq = d.key ? 'k=' + encodeURIComponent(d.key) : '';
  const flag = (d.tls && hub.startsWith('https')) ? '-Lsk' : '-Ls';
  const mkCmd = v => {
    const q = [v ? 'v=' + encodeURIComponent(v) : '', kq].filter(Boolean).join('&');
    return `bash <(curl ${flag} '${hub}/i/${a.id}${q ? '?' + q : ''}')`;
  };
  const dl = (rp, tag, name) => `${hub}/gh/web/${rp.name}/releases/download/${encodeURIComponent(tag)}/${encodeURIComponent(name)}${kq ? '?' + kq : ''}`;
  const repoBlocks = Object.values(d.repos).map(rp => `<div class="card"><h2 class="ltr">${esc(rp.name)} <span class="muted">(شاخهٔ ${esc(rp.default_branch || '?')})</span></h2>
    ${rp.error ? `<div class="err ltr">${esc(rp.error)}</div>` : ''}
    ${(rp.releases || []).map(r => `<h3 class="ltr">${esc(r.tag)} ${r.prerelease ? '<span class="pill warn">pre</span>' : ''} ${r.tag === rp.latest ? '<span class="pill ok">latest</span>' : ''} <span class="muted">${esc(when(r.published))}</span></h3>
      <ul class="assets">${(r.assets || []).map(x => `<li><a href="${esc(dl(rp, r.tag, x.name))}">${esc(x.name)}</a> <span class="muted">${size(x.size)}</span>${/\.apk$/i.test(x.name) ? ' <span class="pill">📱 APK</span>' : ''}</li>`).join('') || '<li class="muted">فایلی آینه نشده</li>'}</ul>`).join('') || '<p class="muted">ریلیزی آینه نشده.</p>'}
    <p class="muted ltr">source: ${esc(hub)}/gh/web/${esc(rp.name)}/archive/refs/heads/${esc(rp.default_branch)}.tar.gz</p></div>`).join('');
  const extRows = d.ext.map(x => `<tr><td class="ltr">${esc(x.url)}</td>
    <td>${x.dynamic ? '<span class="pill">متغیر دارد</span>' : `<label class="inline"><input type="checkbox" data-ext="${esc(x.url)}" ${x.mirror ? 'checked' : ''}> آینه شود</label>`}</td>
    <td>${x.error ? `<span class="pill bad" title="${esc(x.error)}">خطا</span>` : (x.fetched && !x.fetched.startsWith('0001') ? `<span class="pill ok">${size(x.size)}</span>` : '')}</td></tr>`).join('');
  main.innerHTML = `<div class="row"><h1 class="grow">${esc(a.name)} <span class="muted ltr">${esc(a.repo || '')}</span></h1>
    <button id="sync">همگام‌سازی</button><button id="del" class="danger">حذف</button></div>
  ${a.error ? `<div class="card err ltr">${esc(a.error)}</div>` : ''}${a.note ? `<div class="card note">${esc(a.note)}</div>` : ''}
  <div class="card"><h2>دستور نصب روی سرور داخلی</h2>
    <div class="row"><label class="inline">نسخه:</label><select id="ver" class="ltr"><option value="">آخرین (${esc(primary ? (primary.latest || tags[0] || '') : '')})</option>${tags.map(t => `<option>${esc(t)}</option>`).join('')}</select></div>
    <div class="cmd" id="cmdbox"></div>
    <p class="muted">این دستور اسکریپت اصلی را بدون تغییر اجرا می‌کند؛ همهٔ curl/wget های github.com، api.github.com و raw.githubusercontent.com (و وابستگی‌های خارجی آینه‌شده) به این هاب هدایت می‌شوند. متغیر محیطی دلخواه: <code>?NAME=value</code> ، آرگومان: <code>?args=...</code> ، لاگ هدایت‌ها: <code>?trace=1</code></p>
    <p class="muted ltr">original: ${esc(a.command || '—')}</p>
    ${a.recipe ? '<p class="note">⚠️ نسخهٔ ویرایش‌شدهٔ اسکریپت (رسپی) اجرا می‌شود.</p>' : ''}</div>
  <div class="card"><h2>تنظیمات آینه</h2><form id="pf">
    <label>معماری‌ها (هیچ‌کدام = همه)</label>${ARCHS.map(([k, t]) => `<label class="inline"><input type="checkbox" name="arch" value="${k}" ${(a.archs || []).includes(k) ? 'checked' : ''}> ${t}</label>`).join('')}
    <p class="muted">فایل‌های APK و فایل‌های بدون معماری همیشه نگه داشته می‌شوند.</p>
    <div class="row"><div class="grow"><label>تعداد ریلیز (۰ = پیش‌فرض)</label><input name="keep" type="number" min="0" max="50" value="${a.keep || 0}"></div>
    <div class="grow"><label>حذف فایل‌ها با regex</label><input name="exclude" class="ltr" value="${esc(a.exclude || '')}" placeholder="windows|darwin"></div></div>
    <label class="inline"><input type="checkbox" name="pre" ${a.prerelease ? 'checked' : ''}> پیش‌انتشارها (pre-release) هم آینه شوند</label>
    <label>دستور نصب اصلی</label><input name="command" class="ltr" value="${esc(a.command || '')}">
    <p><button>ذخیره</button> <span class="muted">بعد از ذخیره «همگام‌سازی» را بزنید.</span></p></form></div>
  <div class="card"><h2>وابستگی‌های خارجی (${fa(d.ext.length)})</h2>
    ${d.ext.length ? `<table><tr><th>آدرس</th><th>آینه</th><th>وضعیت</th></tr>${extRows}</table>` : '<p class="muted">آدرس غیرگیت‌هابی پیدا نشد.</p>'}
    <p class="muted">فایل ثابت (مثل get.acme.sh) را می‌توان آینه کرد؛ سرویس‌های تشخیص IP و آدرس‌های دارای متغیر قابل آینه‌کردن نیستند.</p></div>
  <div class="card"><h2>ویرایش اسکریپت نصب (رسپی)</h2>
    <p class="muted">اگر اسکریپت را ویرایش کنید، دستور بالا نسخهٔ ویرایش‌شده را اجرا می‌کند (هدایت‌ها همچنان فعال‌اند).</p>
    <div class="row"><button id="loadOrig" class="ghost">بارگذاری اسکریپت اصلی</button><button id="saveRecipe">ذخیرهٔ رسپی</button><button id="clearRecipe" class="ghost">حذف رسپی</button></div>
    <textarea id="recipe" class="code" spellcheck="false">${esc(a.recipe || '')}</textarea></div>
  <h2>دانلودها</h2>${repoBlocks}`;
  const setCmd = () => { const c = mkCmd($('#ver').value); $('#cmdbox').innerHTML = `<span>${esc(c)}</span><button data-copy="${esc(c)}">کپی</button>`; };
  $('#ver').onchange = setCmd; setCmd();
  $('#sync').onclick = async () => { try { await api('apps/' + a.id + '/sync', { method: 'POST' }); toast('همگام‌سازی شروع شد'); watch(); } catch (e) { toast(e.message); } };
  $('#del').onclick = async () => { if (!confirm('حذف ' + a.name + ' و فایل‌هایش؟')) return; await api('apps/' + a.id, { method: 'DELETE' }); location.hash = '#/apps'; };
  $('#pf').onsubmit = async e => {
    e.preventDefault(); const f = e.target;
    const archs = [...f.querySelectorAll('input[name=arch]:checked')].map(x => x.value);
    try { await api('apps/' + a.id, { method: 'PUT', body: { archs, keep: +f.keep.value, exclude: f.exclude.value, prerelease: f.pre.checked, command: f.command.value } }); toast('ذخیره شد'); }
    catch (err) { toast(err.message); }
  };
  main.querySelectorAll('[data-ext]').forEach(cb => cb.onchange = async () => {
    try { await api('ext', { method: 'POST', body: { url: cb.dataset.ext, mirror: cb.checked } }); toast('ذخیره شد؛ همگام‌سازی کنید'); } catch (e) { toast(e.message); }
  });
  $('#loadOrig').onclick = async () => { try { const s = await api('apps/' + a.id + '/script'); $('#recipe').value = s.script; } catch (e) { toast(e.message); } };
  $('#saveRecipe').onclick = async () => { await api('apps/' + a.id, { method: 'PUT', body: { recipe: $('#recipe').value } }); toast('رسپی ذخیره شد'); route(); };
  $('#clearRecipe').onclick = async () => { await api('apps/' + a.id, { method: 'PUT', body: { recipe: '' } }); toast('رسپی حذف شد'); route(); };
  function watch() { let was = true; poll(s => { if (was && !s.sync.running) { clearInterval(pollTimer); route(); } was = s.sync.running; }); }
  const st = await api('status'); if (st.sync.running) watch(); else poll();
}

async function pageCerts() {
  refreshBanner(); poll();
  main.innerHTML = `<h1>گواهی‌ها (CA داخلی)</h1>
  <div class="card"><h2>گواهی ریشهٔ CA</h2>
    <p>این گواهی را روی سرورها و دستگاه‌هایی که باید به سرویس‌های داخلی اعتماد کنند نصب کنید (به‌جای Let's Encrypt).</p>
    <p><a class="btn" href="/ca.crt" download>دانلود ca.crt</a></p>
    <div class="cmd"><span>curl -s ${esc(location.origin)}/ca.crt -o /usr/local/share/ca-certificates/sahand-mirror.crt && update-ca-certificates</span><button data-copy="curl -s ${esc(location.origin)}/ca.crt -o /usr/local/share/ca-certificates/sahand-mirror.crt && update-ca-certificates">کپی</button></div>
    <p class="muted">اندروید: تنظیمات ← امنیت ← نصب گواهی ← CA. مرورگرها هشدار نمی‌دهند پس از نصب.</p></div>
  <div class="card"><h2>صدور گواهی برای یک سرویس</h2><form id="cf">
    <label>IP یا دامنه (چندتا با کاما)</label><input name="hosts" class="ltr" placeholder="10.0.0.12, panel.example.ir">
    <label>مدت اعتبار (روز)</label><input name="days" type="number" value="825" min="1" max="3650">
    <p><button>صدور و دانلود zip</button></p>
    <p class="muted">zip شامل fullchain.pem (گواهی + CA)، privkey.pem و ca.crt است. در 3x-ui مسیر این دو فایل را در «مسیر گواهی/کلید» وارد کنید.</p></form></div>`;
  $('#cf').onsubmit = async e => {
    e.preventDefault();
    try { const r = await api('certs', { method: 'POST', body: { hosts: e.target.hosts.value, days: +e.target.days.value }, raw: true }); saveBlob(await r.blob(), 'cert-' + e.target.hosts.value.split(/[ ,]/)[0] + '.zip'); }
    catch (err) { toast(err.message); }
  };
}

async function pageBundles() {
  refreshBanner(); poll();
  main.innerHTML = `<h1>بسته‌ها (انتقال آفلاین)</h1>
  <div class="card"><h2>خروجی گرفتن از کل آینه</h2>
    <p>یک فایل tar.gz شامل همهٔ ریلیزها، سورس‌ها، فایل‌های خارجی، فهرست برنامه‌ها، باینری هاب و install.sh با manifest و sha256 هر فایل.</p>
    <p><a class="btn" href="/api/bundle/export">دانلود بسته</a></p>
    <div class="cmd"><span>sahand-mirror export --out /root/bundle.tar.gz</span><button data-copy="sahand-mirror export --out /root/bundle.tar.gz">کپی</button></div></div>
  <div class="card"><h2>وارد کردن بسته</h2>
    <p>بسته‌ای که روی سیستمی با اینترنت با <code>sahand-mirror collect</code> یا از هاب دیگر ساخته شده را بارگذاری کنید. همهٔ فایل‌ها با sha256 بررسی می‌شوند.</p>
    <input type="file" id="bf" accept=".gz,.tgz"><div class="progress"><div id="bp"></div></div><p id="bmsg" class="muted"></p>
    <p class="muted">برای فایل‌های خیلی بزرگ بهتر است فایل را با scp روی سرور بگذارید و <code>sahand-mirror import FILE</code> بزنید.</p></div>
  <div class="card"><h2>ساخت بسته روی سیستم دارای اینترنت</h2>
    <div class="cmd"><span>sahand-mirror collect --apps apps.json --out bundle.tar.gz</span><button data-copy="sahand-mirror collect --apps apps.json --out bundle.tar.gz">کپی</button></div>
    <p class="muted"><a href="/api/apps.json" download>apps.json</a> همین هاب را دانلود کنید تا همان برنامه‌ها جمع‌آوری شوند. نصب هاب جدید بدون اینترنت: <code>bash install.sh --offline bundle.tar.gz</code></p></div>`;
  $('#bf').onchange = e => {
    const f = e.target.files[0]; if (!f) return;
    const x = new XMLHttpRequest(); x.open('POST', '/api/bundle/import'); x.setRequestHeader('X-Requested-With', 'sahand-mirror');
    x.upload.onprogress = p => { $('#bp').style.width = (100 * p.loaded / p.total).toFixed(1) + '%'; $('#bmsg').textContent = 'بارگذاری ' + size(p.loaded) + ' از ' + size(p.total); };
    x.onload = () => { let j = {}; try { j = JSON.parse(x.responseText); } catch (e) { } $('#bmsg').textContent = x.status === 200 ? `✅ وارد شد: ${fa(j.files)} فایل، برنامه‌ها: ${(j.apps || []).join('، ')}` : '❌ ' + (j.error || x.status); refreshBanner(); };
    x.onerror = () => { $('#bmsg').textContent = '❌ خطای شبکه'; };
    $('#bmsg').textContent = 'در حال بارگذاری…'; x.send(f);
  };
}

async function pageSettings() {
  refreshBanner(); poll();
  const s = await api('settings');
  main.innerHTML = `<h1>تنظیمات</h1>
  <form class="card" id="sf"><h2>هاب</h2>
    <label>آدرس عمومی (IP داخلی:پورت) — در دستورهای نصب استفاده می‌شود</label><input name="public_url" class="ltr" value="${esc(s.public_url)}" placeholder="http://10.0.0.5:8686">
    <label class="inline"><input type="checkbox" name="direct_sync" ${s.direct_sync ? 'checked' : ''}> همگام‌سازی مستقیم از گیت‌هاب (اگر این سرور به اینترنت بین‌الملل دسترسی دارد)</label>
    <div class="row"><div class="grow"><label>ساعت همگام‌سازی روزانه (۰ تا ۲۳، ‎-1‎ = خاموش)</label><input name="sync_hour" type="number" min="-1" max="23" value="${s.sync_hour}"></div>
    <div class="grow"><label>تعداد ریلیز نگه‌داشته‌شده برای هر مخزن</label><input name="keep" type="number" min="1" max="50" value="${s.keep}"></div></div>
    <label>توکن گیت‌هاب (اختیاری، فقط‌نوشتنی) — ${s.token_set ? '<span class="pill ok">تنظیم شده</span>' : '<span class="pill">خالی</span>'}</label>
    <input name="github_token" class="ltr" type="password" placeholder="${s.token_set ? 'برای تغییر وارد کنید؛ - برای حذف' : 'github_pat_...'}" autocomplete="off">
    <label>کلید دسترسی برای /i ،/gh و /ext (خالی = خاموش)</label>
    <div class="row"><input name="access_key" class="ltr grow" value="${esc(s.access_key)}"><button type="button" id="genKey" class="ghost">ساخت کلید</button></div>
    <label>راهنمای میرور apt/OS (در خروجی نصب نمایش داده می‌شود)</label><input name="apt_mirror_hint" class="ltr" value="${esc(s.apt_mirror_hint)}" placeholder="http://10.0.0.7/ubuntu">
    <label class="inline"><input type="checkbox" name="tls" ${s.tls ? 'checked' : ''}> HTTPS با گواهی CA داخلی (نیاز به ری‌استارت سرویس)</label>
    <label>سقف حجم آپلود بسته (MB)</label><input name="max_upload_mb" type="number" min="1" value="${s.max_upload_mb}">
    <p><button>ذخیره</button></p></form>
  <form class="card" id="pw"><h2>تغییر رمز (${esc(s.admin_user)})</h2>
    <label>رمز فعلی</label><input name="old" type="password" class="ltr" autocomplete="current-password">
    <label>رمز جدید (حداقل ۸ کاراکتر)</label><input name="new" type="password" class="ltr" autocomplete="new-password">
    <p><button>تغییر رمز</button></p></form>`;
  $('#genKey').onclick = () => { const a = new Uint8Array(12); crypto.getRandomValues(a); $('#sf').access_key.value = [...a].map(b => b.toString(16).padStart(2, '0')).join(''); };
  $('#sf').onsubmit = async e => {
    e.preventDefault(); const f = e.target;
    const body = { public_url: f.public_url.value, direct_sync: f.direct_sync.checked, sync_hour: +f.sync_hour.value, keep: +f.keep.value,
      access_key: f.access_key.value, apt_mirror_hint: f.apt_mirror_hint.value, tls: f.tls.checked, max_upload_mb: +f.max_upload_mb.value };
    if (f.github_token.value) body.github_token = f.github_token.value;
    try { await api('settings', { method: 'PUT', body }); toast('ذخیره شد'); route(); } catch (err) { toast(err.message); }
  };
  $('#pw').onsubmit = async e => {
    e.preventDefault();
    try { await api('password', { method: 'POST', body: { old: e.target.old.value, new: e.target.new.value } }); toast('رمز تغییر کرد'); e.target.reset(); } catch (err) { toast(err.message); }
  };
}

// ---------- router ----------
async function route() {
  const h = location.hash.slice(1) || '/';
  $('#top').classList.remove('hidden');
  document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('on', a.getAttribute('href') === '#' + h || (h.startsWith('/app/') && a.getAttribute('href') === '#/apps')));
  try {
    if (h === '/') await pageDashboard();
    else if (h === '/apps') await pageApps();
    else if (h.startsWith('/app/')) await pageApp(decodeURIComponent(h.slice(5)));
    else if (h === '/certs') await pageCerts();
    else if (h === '/bundles') await pageBundles();
    else if (h === '/settings') await pageSettings();
    else location.hash = '#/';
  } catch (e) { if (e.message !== 'login') main.innerHTML = `<div class="card err">${esc(e.message)}</div>`; }
}
window.addEventListener('hashchange', route);
$('#logout').onclick = async () => { await api('logout', { method: 'POST' }).catch(() => { }); showLogin(); };
route();
