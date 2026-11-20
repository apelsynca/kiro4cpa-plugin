const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, '../internal/kiro/connect_page.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const future = () => new Date(Date.now() + 600000).toISOString();
const pending = (extra = {}) => ({status: 'pending', method: 'social', expiresAt: future(), url: 'https://app.kiro.dev/signin?state=test', ...extra});
const response = (data, status = 200) => ({ok: status < 400, status, json: async () => data});
const settle = () => new Promise(setImmediate);

// Mirrors official v1.27.3 encryption.ts, including UTF-8 and host binding.
function officialStorage(state, host = 'example.test') {
  const key = Buffer.from('cli-proxy-api-webui::secure-storage|' + host);
  const bytes = Buffer.from(JSON.stringify({state, version: 0}));
  return 'enc::v2::' + Buffer.from(bytes.map((b, i) => b ^ key[i % key.length])).toString('base64');
}

function page(fetcher, saved = true, options = {}) {
  const elements = new Map();
  for (const match of html.matchAll(/<([a-z]+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
    const [, tag, attrs, id] = match;
    const classes = new Set((attrs.match(/class="([^"]*)"/)?.[1] || '').split(/\s+/).filter(Boolean));
    const handlers = {};
    elements.set(id, {id, tag, value: attrs.match(/value="([^"]*)"/)?.[1] || (tag === 'select' ? 'social' : ''),
      textContent: '', disabled: false, style: {}, dataset: {}, handlers,
      classList: {add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c),
        toggle(c, on) { const show = on === undefined ? !classes.has(c) : on; show ? classes.add(c) : classes.delete(c); }},
      setAttribute(name, value) {this[name] = value;}, addEventListener(name, fn) {handlers[name] = fn;}});
  }
  const methods = ['builder', 'social', 'idc', 'import'].map(pane => ({dataset: {pane}, disabled: false,
    setAttribute(name, value) {this[name] = value;}, addEventListener() {}}));
  let timerID = 0, calls = [];
  const timers = new Map();
  const get = id => {assert.ok(elements.has(id), `missing DOM element ${id}`); return elements.get(id);};
  const storage = {'cli-proxy-auth': saved ? officialStorage({managementKey: 'test-key', rememberPassword: true, apiBase: 'https://example.test'}) : null, ...options.storage};
  const listeners = {}, media = {matches: Boolean(options.dark), addEventListener(name, fn) {this.listener = fn;}};
  const root = {lang: html.match(/<html\b[^>]*\blang="([^"]+)"/)?.[1],
    setAttribute(name, value) {this[name] = value;}, removeAttribute(name) {delete this[name];}};
  const context = {URL, URLSearchParams, TextEncoder, TextDecoder, Uint8Array, Date, Number, atob,

    location: {hash: '#state=example', host: 'example.test', origin: 'https://example.test', href: 'https://example.test/connect'},
    navigator: {language: options.language || 'en', clipboard: {writeText: async () => {}}}, window: {matchMedia: () => media, addEventListener(name, fn) {listeners[name] = fn;}},
    localStorage: {getItem(key) {if (options.storageBlocked) throw new Error('blocked'); return storage[key] ?? null;}},
    document: {documentElement: root, getElementById: get, querySelectorAll(selector) {
      if (selector === '.method') return methods;
      if (selector === '.pane') return ['builder', 'social', 'idc', 'import'].map(p => get('pane-' + p));
      if (selector === '.pane button') return [...elements.values()].filter(n => n.tag === 'button' && /^(btn-(builder|idc|social|import)|copy-)/.test(n.id));
      throw new Error('unexpected selector ' + selector);
    }},
    setTimeout(fn) {const id = ++timerID; timers.set(id, {fn, once: true}); return id;},
    clearTimeout(id) {timers.delete(id);},
    setInterval(fn) {const id = ++timerID; timers.set(id, {fn, once: false}); return id;},
    clearInterval(id) {timers.delete(id);},
    async fetch(url, opts) {calls.push({url, opts}); if (url.startsWith('/v8/management/oauth/status?')) return options.hostFetch ? options.hostFetch(url, opts) : response({status: 'ok'}); return fetcher(calls.length, url, opts);}
  };
  vm.runInNewContext(script, context);
  return {get, methods, calls, root, media, storage, listeners,
    async click(id) {await get(id).handlers.click.call(get(id)); await settle();},
    async tick() {const entry = [...timers].find(([, v]) => v.once); assert.ok(entry, 'poll scheduled'); timers.delete(entry[0]); await entry[1].fn(); await settle();},
    polls() {return [...timers.values()].filter(v => v.once).length;}};
}

test('a failed manual key stops polling until explicit correction', async () => {
  const p = page(n => response(n === 2 ? {} : pending(), n === 2 ? 401 : 200), false);
  assert.equal(p.calls.length, 0);
  p.get('mgmt-key').value = 'bad-key'; await p.click('btn-key-resume'); await p.tick();
  assert.equal(p.calls.length, 2); assert.equal(p.polls(), 0); assert.equal(p.get('mgmt-key').value, '');
  assert.match(p.get('msg').textContent, /Invalid management key/);
  await p.click('btn-social-complete'); assert.equal(p.calls.length, 2);
  p.get('mgmt-key').value = 'corrected'; await p.click('btn-key-resume');
  assert.equal(p.calls.length, 3); assert.equal(p.polls(), 1);
});
test('403 pauses with an access restriction explanation', async () => {
  const p = page(() => response({}, 403)); await settle();
  assert.equal(p.polls(), 0); assert.match(p.get('msg').textContent, /access restriction/);
  assert.equal(p.get('key-box').style.display, 'block');
});
test('reload restores social login pane and callback entry', async () => {
  const p = page(() => response(pending())); await settle();
  assert.equal(p.get('pane-social').classList.contains('active'), true);
  assert.equal(p.get('social-paste').classList.contains('hidden'), false);
  assert.match(p.get('link-social').href, /^https:\/\/app.kiro.dev/);
  assert.ok(p.methods.every(b => b.disabled)); assert.equal(p.polls(), 1);
});
test('reload restores device authorization and countdown', async () => {
  const p = page(() => response(pending({method: 'device-idc', verificationUri: 'https://view.awsapps.com/start', userCode: 'ABCD-EFGH', deviceExpiresAt: future(), region: 'eu-central-1', startUrl: 'https://d-test.awsapps.com/start'})));
  await settle();
  assert.equal(p.get('code-idc').textContent, 'ABCD-EFGH'); assert.equal(p.get('btn-idc').disabled, true);
  assert.equal(p.get('device-idc').classList.contains('hidden'), false);
  assert.equal(p.get('idc-region').value, 'eu-central-1'); assert.match(p.get('countdown').textContent, /^Time remaining: \d+m \d+s$/);
});
test('three consecutive network failures stop rather than spin silently', async () => {
  const p = page(n => {if (n > 1) throw new Error('offline'); return response(pending());});
  await settle(); for (let i = 0; i < 3; i++) await p.tick();
  assert.equal(p.calls.length, 4); assert.equal(p.polls(), 0);
  assert.match(p.get('msg').textContent, /offline/); assert.equal(p.get('btn-resume').classList.contains('hidden'), false);
});
test('slow poll cannot overlap another poll or a login action', async () => {
  let release;
  const p = page(n => n === 1 ? response(pending()) : new Promise(resolve => {release = resolve;}));
  await settle(); const tick = p.tick(); await settle();
  assert.equal(p.polls(), 0); await p.click('btn-builder'); assert.equal(p.calls.length, 2);
  release(response(pending())); await tick; assert.equal(p.polls(), 1);
});
test('terminal device failure offers a fresh panel login', async () => {
  const p = page(() => response({status: 'error', error: 'AWS authorization expired', method: 'device-builder'})); await settle();
  assert.equal(p.polls(), 0); assert.equal(p.get('btn-restart').classList.contains('hidden'), false);
  assert.match(p.get('msg').textContent, /AWS authorization expired\. Return to the panel to start a new Kiro sign-in session\./);
});
test('invalid imports retain the validation warning after the host saves them', async () => {
  const p = page(() => response({status: 'success', validation: 'invalid', warning: 'The credentials are no longer valid.', delivered: false})); await settle();
  assert.equal(p.get('msg').className, 'msg warn');
  assert.match(p.get('msg').textContent, /The credentials are no longer valid\. Credentials saved\./); assert.equal(p.polls(), 0);
});
test('expired session stops all future requests', async () => {
  const p = page(() => response({error: 'Sign-in session not found or expired'}, 404)); await settle();
  assert.equal(p.polls(), 0); assert.ok(p.methods.every(b => b.disabled));
});


test('official remembered UTF-8 key is decoded without storing or exposing it', async () => {
  const key = 'test-cl\u00e9-\u{1f511}';
  const p = page(() => response(pending()), true, {storage: {'cli-proxy-auth': officialStorage({managementKey: key, rememberPassword: true, apiBase: 'https://example.test'})}});
  await settle();
  assert.equal(p.calls.length, 1);
  assert.equal(p.calls[0].opts.headers.Authorization, 'Bearer ' + key);
  assert.equal(p.get('mgmt-key').value, '');
});

test('retired, cross-server, unremembered and corrupt credentials require manual entry', () => {
  for (const raw of [
    JSON.stringify({state: {managementKey: 'old'}}),
    'enc::v1::old', 'enc::v2::invalid',
    officialStorage({managementKey: 'other', rememberPassword: true, apiBase: 'https://other.test'}),
    officialStorage({managementKey: 'forgotten', rememberPassword: false}),
    officialStorage({managementKey: 'wrong-host', rememberPassword: true}, 'other.test')
  ]) {
    const p = page(() => {throw new Error('must not send');}, true, {storage: {'cli-proxy-auth': raw, managementKey: 'legacy'}});
    assert.equal(p.calls.length, 0); assert.equal(p.get('key-box').style.display, 'block');
  }
});

test('official theme preference overrides system appearance and follows changes', () => {
  for (const [theme, dark, expected] of [['light', true, undefined], ['white', true, 'white'], ['dark', false, 'dark'], ['auto', false, 'white'], ['auto', true, 'dark']]) {
    const p = page(() => response(pending()), false, {dark, storage: {'cli-proxy-theme': JSON.stringify({state: {theme}})}});
    assert.equal(p.root['data-theme'], expected);
    p.media.matches = !dark; p.media.listener();
    assert.equal(p.root['data-theme'], theme === 'auto' ? (dark ? 'white' : 'dark') : expected);
    p.storage['cli-proxy-theme'] = JSON.stringify({state: {theme: 'dark'}}); p.listeners.storage({key: 'cli-proxy-theme'});
    assert.equal(p.root['data-theme'], 'dark');
  }
});

test('page stays in English regardless of panel and browser locales', () => {
  assert.match(html, /<title>Kiro sign in<\/title>/);
  assert.match(html, /id="btn-key-resume"[^>]*>Verify key and continue<\/button>/);
  for (const options of [
    {language: 'ru', storage: {'cli-proxy-language': JSON.stringify({state: {language: 'zh-TW'}})}},
    {language: 'zh-CN', storage: {'cli-proxy-language': JSON.stringify({state: {language: 'ru'}})}},
    {language: 'zh-HK', storageBlocked: true, dark: true}
  ]) {
    const p = page(() => response(pending()), false, options);
    assert.equal(p.root.lang, 'en');
    assert.match(p.get('msg').textContent, /^Enter your management key/);
    if (options.dark) assert.equal(p.root['data-theme'], 'dark');
  }
});

test('return navigation works with no opener after success and failure', async () => {
  assert.match(html, /id="btn-restart"[^>]*href="\/management\.html#\/oauth"/);
  for (const data of [{status: 'success', delivered: true}, {status: 'error', error: 'expired'}]) {
    const p = page(() => response(data)); await settle();
    assert.equal(p.get('btn-restart').tag, 'a'); assert.equal(p.get('btn-restart').classList.contains('hidden'), false);
  }
});


test('credentials are only reported saved after the official host confirms', async () => {
  let hostCalls = 0;
  const p = page(() => response({status: 'success', delivered: true}), true, {hostFetch: (url, opts) => {
    hostCalls++;
    assert.equal(url, '/v8/management/oauth/status?state=example');
    assert.equal(opts.headers.Authorization, 'Bearer test-key');
    return response({status: hostCalls === 1 ? 'wait' : 'ok'});
  }});
  await settle();
  assert.equal(hostCalls, 1); assert.equal(p.polls(), 1);
  assert.doesNotMatch(p.get('msg').textContent, /Credentials saved/);
  await p.tick(); assert.equal(hostCalls, 2); assert.equal(p.polls(), 0);
  assert.match(p.get('msg').textContent, /Credentials saved/);
});

test('host save failure is shown even after plugin completion', async () => {
  const p = page(() => response({status: 'success', delivered: true}), true, {hostFetch: () => response({status: 'error', error: 'Failed to save authentication tokens'})});
  await settle();
  assert.equal(p.get('msg').className, 'msg err'); assert.match(p.get('msg').textContent, /Failed to save/);
  assert.equal(p.polls(), 0); assert.equal(p.get('btn-restart').classList.contains('hidden'), false);
});

test('host confirmation also pauses on 401 and can resume after key correction', async () => {
  let hostCalls = 0;
  const p = page(() => response({status: 'success'}), true, {hostFetch: () => ++hostCalls === 1 ? response({}, 401) : response({status: 'ok'})});
  await settle(); assert.equal(p.polls(), 0); assert.equal(hostCalls, 1);
  p.get('mgmt-key').value = 'corrected'; await p.click('btn-key-resume');
  assert.equal(hostCalls, 2); assert.match(p.get('msg').textContent, /Credentials saved/);
});

test('a failed host confirmation never enables importing the same credentials twice', async () => {
  const p = page((n, url, opts) => response(opts.method === 'POST' ? {status: 'success'} : pending({method: ''})), true, {hostFetch: () => {throw new Error('offline');}});
  await settle();
  await p.click('btn-import');
  assert.equal(p.get('btn-import').disabled, true);
  const count = p.calls.length;
  await p.click('btn-import'); assert.equal(p.calls.length, count);
  assert.equal(p.get('btn-resume').classList.contains('hidden'), false);
});

test('an unexpected host response cannot report credentials saved', async () => {
  const p = page(() => response({status: 'success'}), true, {hostFetch: () => response({})});
  await settle();
  assert.equal(p.get('msg').className, 'msg err'); assert.doesNotMatch(p.get('msg').textContent, /Credentials saved/);
  assert.equal(p.get('btn-resume').classList.contains('hidden'), false);
});
