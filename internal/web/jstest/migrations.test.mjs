// Unit tests for the one-time data migrations (settings.js). Zero deps:
//   node internal/web/jstest/migrations.test.mjs
// Also wired into `go test ./internal/web` (TestJSMigrations).
//
// Two rules these guard, both learned the hard way:
//  - a single-user client's loopback port changes between launches and wipes
//    localStorage, so its applied level must live on the server, or every
//    migration replays forever;
//  - a shared backend serves one profiles.json to every visitor, but these
//    migrations clean per-browser localStorage — so there the level must be
//    per-browser, and no visitor may write global settings for the others.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const jsDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'static', 'js');

let failed = 0, passed = 0;
function check(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g === w) { passed++; return; }
  failed++;
  console.error(`FAIL ${name}\n  got:  ${g}\n  want: ${w}`);
}

// Pull just the migration helpers out of settings.js — evaluating the whole
// file would run its page setup, which needs a real DOM.
function extract(src, names) {
  let out = '';
  for (const name of names) {
    const decl = src.indexOf(`function ${name}(`);
    const arr = src.indexOf(`var ${name} = [`);
    const start = decl !== -1 ? decl : arr;
    if (start === -1) throw new Error(`settings.js: ${name} not found`);
    if (decl !== -1) {
      let depth = 0;
      for (let j = src.indexOf('{', start); j < src.length; j++) {
        if (src[j] === '{') depth++;
        else if (src[j] === '}' && --depth === 0) { out += src.slice(start, j + 1) + '\n'; break; }
      }
    } else {
      let depth = 0;
      for (let j = src.indexOf('[', start); j < src.length; j++) {
        if (src[j] === '[') depth++;
        else if (src[j] === ']' && --depth === 0) { out += src.slice(start, j + 1) + ';\n'; break; }
      }
    }
  }
  return out;
}
const src = readFileSync(join(jsDir, 'settings.js'), 'utf8');
const code = extract(src, ['TF_MIGRATIONS', 'tfLocalMigrationLevel', 'runPendingMigrations', 'pruneVersionKeys']);

function setupEnv(seed = {}) {
  const store = Object.assign({}, seed);
  const state = { posts: [] };
  globalThis.localStorage = {
    getItem: (k) => (k in store ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: (k) => { delete store[k]; },
  };
  // Object.keys(localStorage) must see the stored keys, as it does in a browser.
  globalThis.localStorage = new Proxy(globalThis.localStorage, {
    ownKeys: () => Reflect.ownKeys(store),
    getOwnPropertyDescriptor: (t, k) =>
      (k in store ? { enumerable: true, configurable: true, value: store[k] } : Reflect.getOwnPropertyDescriptor(t, k)),
  });
  globalThis.fetch = (u, o) => {
    if (String(u).includes('/api/settings') && o && o.method === 'POST') state.posts.push(JSON.parse(o.body));
    return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
  };
  vm.runInThisContext(code, { filename: 'settings.js:migrations' });
  return { store, state };
}

const LEVEL = (() => { setupEnv(); return globalThis.TF_MIGRATIONS.length; })();
check('there is at least one migration', LEVEL >= 1, true);

// ===== single-user client: level lives on the server =========================
{
  const { store, state } = setupEnv({
    'thefeed_skip_gh_update_0.34.0': '1',
    'thefeed_skip_gh_update_0.20.0': '1',
  });
  await globalThis.runPendingMigrations({ migrationVersion: 0, shared: false });
  check('m1 clears the legacy local skip keys',
    Object.keys(store).filter(k => k.startsWith('thefeed_skip_gh_update_')), []);
  check('m1 clears the server skip and records the level',
    state.posts, [{ migrationVersion: LEVEL, skipUpdateVersion: '' }]);
}

// Already at the current level: nothing runs, nothing is written.
{
  const { store, state } = setupEnv({ 'thefeed_skip_gh_update_0.34.0': '1' });
  await globalThis.runPendingMigrations({ migrationVersion: LEVEL, shared: false });
  check('a migrated install posts nothing', state.posts, []);
  check('a migrated install keeps its skip', store['thefeed_skip_gh_update_0.34.0'], '1');
}

// A level from the future must not make anything run again.
{
  const { state } = setupEnv();
  await globalThis.runPendingMigrations({ migrationVersion: LEVEL + 5, shared: false });
  check('a newer level than we know posts nothing', state.posts, []);
}

// ===== shared backend: level is per browser, no global writes ================
{
  const { store, state } = setupEnv({ 'thefeed_skip_gh_update_0.34.0': '1' });
  await globalThis.runPendingMigrations({ migrationVersion: 0, shared: true });
  check('shared mode still cleans this browser',
    Object.keys(store).filter(k => k.startsWith('thefeed_skip_gh_update_')), []);
  check('shared mode writes no global settings', state.posts, []);
  check('shared mode records the level locally', store['thefeed_migration_level'], String(LEVEL));

  await globalThis.runPendingMigrations({ migrationVersion: 0, shared: true });
  check('shared mode does not repeat for this browser', state.posts, []);
}

// The server level must NOT satisfy a shared visitor — their localStorage is
// still dirty even though someone else already migrated.
{
  const { store } = setupEnv({ 'thefeed_skip_gh_update_0.34.0': '1' });
  await globalThis.runPendingMigrations({ migrationVersion: LEVEL, shared: true });
  check('shared visitor migrates despite the server level being current',
    Object.keys(store).filter(k => k.startsWith('thefeed_skip_gh_update_')), []);
}

// ===== version keys must not accumulate one per release =====================
{
  const { store } = setupEnv({
    'thefeed_skip_gh_update_0.10.0': '1',
    'thefeed_skip_gh_update_0.20.0': '1',
    'thefeed_skip_gh_update_0.30.0': '1',
    'unrelated_key': 'keep me',
  });
  globalThis.pruneVersionKeys('thefeed_skip_gh_update_', 'thefeed_skip_gh_update_0.30.0');
  check('only the kept version survives',
    Object.keys(store).filter(k => k.startsWith('thefeed_skip_gh_update_')),
    ['thefeed_skip_gh_update_0.30.0']);
  check('unrelated keys are untouched', store['unrelated_key'], 'keep me');
}

// A migration that throws must not stop the others or lose the level.
{
  const { state } = setupEnv();
  globalThis.TF_MIGRATIONS.unshift(function () { throw new Error('boom'); });
  await globalThis.runPendingMigrations({ migrationVersion: 0, shared: false });
  check('a throwing migration still records the level',
    state.posts.length === 1 && state.posts[0].migrationVersion === globalThis.TF_MIGRATIONS.length, true);
}

console.log(`${passed} passed, ${failed} failed  (${LEVEL} migration${LEVEL === 1 ? '' : 's'})`);
process.exitCode = failed ? 1 : 0;
