// Unit tests for triggerDownload (core.js). Zero dependencies — run with:
//   node internal/web/jstest/download.test.mjs
// Also wired into `go test ./internal/web` (TestJSDownloadUnit).
//
// Regression under test: saves used to go through navigator.share, which needs
// transient user activation — already gone once the export fetch resolves. The
// rejection was swallowed and the code returned early, so nothing was saved:
// no file, no dialog, no error (desktop Chrome), and the iOS/macOS save buttons
// did nothing. Saves now use the native bridge (Android AND iOS) or a plain
// <a download>; navigator.share is not involved.
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

// Pull only the download helpers out of core.js — running the whole file would
// execute its top-level page setup, which needs a real DOM.
function extractFns(src, names) {
  let out = '';
  for (const name of names) {
    const start = src.indexOf(`function ${name}(`);
    if (start === -1) throw new Error(`core.js: function ${name} not found`);
    let depth = 0, i = src.indexOf('{', start);
    for (let j = i; j < src.length; j++) {
      if (src[j] === '{') depth++;
      else if (src[j] === '}' && --depth === 0) { out += src.slice(start, j + 1) + '\n'; break; }
    }
  }
  return out;
}
const coreSrc = readFileSync(join(jsDir, 'core.js'), 'utf8');
const downloadSrc = extractFns(coreSrc, ['triggerDownload', 'anchorDownload']);

// --- Harness: a fresh fake environment per scenario -------------------------
// Records whether an <a download> click happened and whether share() was used.
function setupEnv({ share, canShare, canShareFiles, bridge, readerFails, saveResult }) {
  const state = { anchorClicks: [], shared: 0, bridgeSaves: [], toasts: [] };

  const anchor = () => ({
    href: '', download: '', click() { state.anchorClicks.push(this.download); },
    remove() { },
  });

  globalThis.document = {
    addEventListener() { },
    getElementById() { return null; },
    createElement() { return anchor(); },
    body: { appendChild() { } },
  };
  globalThis.window = globalThis;
  globalThis.setTimeout = () => 0;
  globalThis.URL = { createObjectURL: () => 'blob:x', revokeObjectURL() { } };
  globalThis.File = class { constructor(parts, name, opts) { this.name = name; this.type = (opts || {}).type; } };
  globalThis.showToast = m => { state.toasts.push(m); };
  globalThis.t = k => k;
  globalThis.FileReader = class {
    readAsDataURL(b) {
      if (readerFails) { this.onerror(); return; }
      this.result = 'data:' + (b.type || '') + ';base64,QkFTRTY0';
      this.onload();
    }
  };
  const nativeBridge = {
    saveMedia(b64, mime, name) { state.bridgeSaves.push({ name, mime, b64 }); return saveResult; }
  };
  globalThis.Android = bridge === 'android' ? nativeBridge : undefined;
  globalThis.IOS = bridge === 'ios' ? nativeBridge : undefined;

  const nav = { userAgent: 'test', platform: 'test', maxTouchPoints: 0 };
  if (share) {
    // Present but must never be used: it is activation-gated and unreliable.
    nav.share = () => { state.shared++; return Promise.resolve(); };
  }
  if (canShare) nav.canShare = () => !!canShareFiles;
  // node exposes a read-only global navigator; override it explicitly.
  Object.defineProperty(globalThis, 'navigator', { value: nav, configurable: true, writable: true });

  vm.runInThisContext(downloadSrc, { filename: 'core.js:triggerDownload' });
  return state;
}

const blob = { type: 'application/octet-stream' };
const flush = () => new Promise(r => setImmediate(r));

// ===== Desktop Chrome (the reported Windows bug) =====
// It exposes share/canShare, but the file must still be downloaded normally.
{
  const st = setupEnv({ share: true, canShare: true, canShareFiles: true });
  globalThis.triggerDownload(blob, 'thefeed-backup.tfbak');
  await flush();
  check('desktop: downloads the file', st.anchorClicks, ['thefeed-backup.tfbak']);
  check('desktop: never uses the share sheet', st.shared, 0);
}

// ===== Desktop browser without the Share API at all =====
{
  const st = setupEnv({ share: false, canShare: false });
  globalThis.triggerDownload(blob, 'a.tfbak');
  await flush();
  check('no share API: downloads', st.anchorClicks, ['a.tfbak']);
}

// ===== iOS / macOS app: must hand the bytes to the native save bridge =====
// Previously this went through navigator.share, so the save button did
// nothing on iOS and could not save at all on macOS.
{
  const st = setupEnv({ share: true, canShare: true, canShareFiles: true, bridge: 'ios' });
  globalThis.triggerDownload(blob, 'b.tfbak');
  await flush();
  check('iOS: saves via the native bridge', st.bridgeSaves.map(s => s.name), ['b.tfbak']);
  check('iOS: never uses the share sheet', st.shared, 0);
  check('iOS: no anchor download (it is ignored there)', st.anchorClicks, []);
}

// ===== Android app: unchanged, still uses its bridge =====
{
  const st = setupEnv({ share: false, canShare: false, bridge: 'android' });
  globalThis.triggerDownload(blob, 'c.jpg');
  await flush();
  check('Android: saves via the native bridge', st.bridgeSaves.map(s => s.name), ['c.jpg']);
  check('Android: no anchor download', st.anchorClicks, []);
}

// ===== The bridge receives a usable mime type =====
{
  const st = setupEnv({ share: false, canShare: false, bridge: 'ios' });
  globalThis.triggerDownload({ type: '' }, 'd.bin');
  await flush();
  check('bridge gets a fallback mime', st.bridgeSaves.map(s => s.mime), ['application/octet-stream']);
}

// ===== An extensionless name keeps its name =====
// octet-stream is not an extension: the update asset thefeed-client-android-arm64
// was being saved as thefeed-client-android-arm64.octet-stream, which no OS runs.
{
  const st = setupEnv({ share: false, canShare: false, bridge: 'android' });
  globalThis.triggerDownload(blob, 'thefeed-client-android-arm64');
  await flush();
  check('octet-stream adds no extension', st.bridgeSaves.map(s => s.name),
    ['thefeed-client-android-arm64']);
}

// ===== The caller can tell a failed save from a good one =====
// Callers persist "already downloaded" state on success, so an unreported
// failure silently suppresses the update prompt for a file that never landed.
{
  const st = setupEnv({ share: false, canShare: false, bridge: 'android', saveResult: false });
  check('bridge failure resolves false', await globalThis.triggerDownload(blob, 'e.bin'), false);
  check('bridge failure still attempted the save', st.bridgeSaves.length, 1);
}
{
  const st = setupEnv({ share: false, canShare: false, bridge: 'ios', readerFails: true });
  check('reader error resolves false', await globalThis.triggerDownload(blob, 'f.bin'), false);
  // Translated, not a hardcoded English string: t() is stubbed to echo the key.
  check('reader error tells the user', st.toasts, ['save_failed']);
}
{
  setupEnv({ share: false, canShare: false, bridge: 'ios' });
  check('iOS undefined return counts as saved', await globalThis.triggerDownload(blob, 'g.bin'), true);
}
{
  setupEnv({ share: false, canShare: false });
  check('anchor path resolves true', await globalThis.triggerDownload(blob, 'h.bin'), true);
}

console.log(`${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
