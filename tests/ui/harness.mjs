// The world the UI tests run in, so a result never depends on live models:
//
// - a fake provider served from the test process: alpha (no key, five-model world
//   with beta), beta (no key, may train on prompts) and gamma (needs the key
//   "good-key"; not in use until a key is added, then also serves coder-lite);
//   answers after 800 ms, long enough to see a check running; "think" models reason;
// - a router started from the given binary in a fresh temporary folder, with no
//   background checks or tests;
// - headless Chrome with one page, driven over the DevTools protocol.
//
// Nothing outside the temporary folder is touched, and it is removed at the end.

import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, writeFileSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
export const GOOD_KEY = "good-key";

const MODELS = {
  a: ["coder-big", "chat-small", "think-pro"],
  b: ["vision-mini", "coder-lite"],
  c: ["coder-lite", "gamma-only"],
};

export async function startWorld(script) {
  const [bin, chromeArg] = process.argv.slice(2);
  if (!bin) { console.error(`usage: ${script} <router binary> [chrome binary]`); process.exit(2); }
  const chrome = chromeArg || [
    "C:/Program Files/Google/Chrome/Application/chrome.exe",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
  ].find(existsSync);
  if (!chrome) { console.error("no Chrome found; pass its path"); process.exit(2); }

  const dir = mkdtempSync(join(tmpdir(), "llmr-ui-"));
  const children = [];
  let failed = 0, passed = 0;
  const check = (name, ok, detail = "") => {
    if (ok) passed++; else failed++;
    console.log(`${ok ? "[OK]  " : "[FAIL]"} ${name}${!ok && detail ? `\n         ${detail}` : ""}`);
  };

  // the fake provider
  const fake = createServer((req, res) => {
    const [, p] = req.url.split("/");
    if (p === "c" && req.headers.authorization !== `Bearer ${GOOD_KEY}`) {
      res.statusCode = 401;
      return res.end(JSON.stringify({ error: { message: "invalid api key" } }));
    }
    if (req.url.endsWith("/models")) {
      res.setHeader("Content-Type", "application/json");
      return res.end(JSON.stringify({ data: (MODELS[p] || []).map((id) => ({ id })) }));
    }
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", async () => {
      await sleep(800);
      const j = JSON.parse(body || "{}");
      const think = String(j.model).includes("think");
      // "Repeat: X" comes back as X, so a test can see what the model got and answered
      const said = String(j.messages?.at(-1)?.content || ""), reply = said.startsWith("Repeat: ") ? said.slice(8) : "ok";
      if (j.stream) {
        res.setHeader("Content-Type", "text/event-stream");
        if (think) res.write(`data: ${JSON.stringify({ choices: [{ delta: { reasoning_content: "hmm" } }] })}\n\n`);
        res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: reply } }] })}\n\n`);
        res.write(`data: ${JSON.stringify({ choices: [{ delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 5, completion_tokens: 1 } })}\n\n`);
        return res.end("data: [DONE]\n\n");
      }
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ choices: [{ message: { role: "assistant", content: reply, ...(think ? { reasoning_content: "hmm" } : {}) }, finish_reason: "stop" }],
        usage: { prompt_tokens: 5, completion_tokens: 1 } }));
    });
  });
  await new Promise((r) => fake.listen(0, "127.0.0.1", r));
  const fakeURL = `http://127.0.0.1:${fake.address().port}`;
  const T = true, F = false;
  writeFileSync(join(dir, "seed.json"), JSON.stringify({ providers: [
    { id: "alpha", name: "Alpha", base_url: `${fakeURL}/a/v1`, no_key: true, trains_on_free: false, discover: "seed", models: [
      { id: "coder-big", tier: 1, context: 262144, tools: T, vision: T },
      { id: "chat-small", tier: 3, context: 32000, tools: F, vision: F },
      { id: "think-pro", tier: 2, context: 131072, tools: T } ] },
    { id: "beta", name: "Beta", base_url: `${fakeURL}/b/v1`, no_key: true, trains_on_free: true, discover: "seed", models: [
      { id: "vision-mini", tier: 2, context: 128000, tools: F, vision: T },
      { id: "coder-lite", tier: 3, context: 64000, tools: T } ] },
    { id: "gamma", name: "Gamma", base_url: `${fakeURL}/c/v1`, key_env: ["GAMMA_TEST_KEY"], trains_on_free: false, discover: "seed",
      signup_url: "https://example.com/keys", models: [
      { id: "coder-lite", tier: 3, context: 64000, tools: T },
      { id: "gamma-only", tier: 2, context: 64000, tools: T } ] },
  ] }));

  // the router
  const port = 18890 + Math.floor(Math.random() * 100);
  const base = `http://127.0.0.1:${port}`;
  const env = { ...process.env, ROUTER_ADDR: `127.0.0.1:${port}`, ROUTER_SEED: join(dir, "seed.json"),
    ROUTER_PROFILES: join(dir, "profiles.json"), ROUTER_STATE: join(dir, "state.json"), ROUTER_KEYS: join(dir, "keys.dat"),
    ROUTER_MASTER_KEY_FILE: join(dir, "master.key"), ROUTER_TRAFFIC_DIR: join(dir, "logs"), ROUTER_FILTER: join(dir, "filter.json"), ROUTER_ENTITIES: join(dir, "entities.json"), HOME: dir, USERPROFILE: dir,
    ROUTER_PROBE: "0", ROUTER_TEST: "0", ROUTER_ENV_FILE: join(dir, "none.env"), GAMMA_TEST_KEY: "" };
  children.push(spawn(bin, ["serve"], { env, stdio: "ignore" }));
  for (let i = 0; i < 100; i++) { try { if ((await fetch(base + "/health")).ok) break; } catch {} await sleep(100); }

  // Chrome
  const cdpPort = 9400 + Math.floor(Math.random() * 100);
  children.push(spawn(chrome, ["--headless=new", "--disable-gpu", `--remote-debugging-port=${cdpPort}`, `--user-data-dir=${join(dir, "chrome")}`,
    "--no-first-run", "--window-size=1440,1000", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] }));
  let chromeErr = "";
  children.at(-1).stderr.on("data", (d) => { chromeErr += d; });
  let target;
  for (let i = 0; i < 300 && !target; i++) { try { target = (await (await fetch(`http://127.0.0.1:${cdpPort}/json/list`)).json()).find((t) => t.type === "page"); } catch {} await sleep(100); }
  // say why instead of failing on an undefined target
  if (!target) { console.error("Chrome did not start:\n" + chromeErr.slice(-2000)); children.forEach((c) => c.kill()); process.exit(1); }
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r) => ws.addEventListener("open", r));
  let seq = 0; const waiting = new Map();
  ws.addEventListener("message", (m) => { const d = JSON.parse(m.data); if (waiting.has(d.id)) { waiting.get(d.id)(d); waiting.delete(d.id); } });
  const cdp = (method, params = {}) => new Promise((r) => { const i = ++seq; waiting.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
  await cdp("Page.enable"); await cdp("Runtime.enable");

  async function js(expr) {
    const r = await cdp("Runtime.evaluate", { expression: `(async () => { ${expr} })()`, returnByValue: true, awaitPromise: true });
    if (r.result?.exceptionDetails) throw new Error(r.result.exceptionDetails.exception?.description || "page error");
    return r.result?.result?.value;
  }
  const until = async (expr, ms = 8000) => { for (let t = 0; t < ms; t += 100) { try { if (await js(`return !!(${expr})`)) return true; } catch {} await sleep(100); } return false; };
  // open a page, wait until `ready` holds, then define the helpers every test uses
  async function open(path, ready) {
    await cdp("Page.navigate", { url: base + path });
    await sleep(300);
    await until(ready, 10000);
    await js(`window.confirm = () => true;   // a plain confirm asks first; the test says yes
      window.fire = (el, ev = "change") => el.dispatchEvent(new Event(ev, { bubbles: true }));
      window.press = (el) => el.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, button: 0 }));
      window.$$ = (q) => document.querySelector(q);`);
  }
  const get = async (path) => (await fetch(base + path)).json();

  async function finish() {
    ws.close();
    for (const c of children) try { c.kill(); } catch {}
    fake.close();
    await sleep(500);
    try { rmSync(dir, { recursive: true, force: true }); } catch {}
    console.log(`\n${passed} passed, ${failed} failed`);
    process.exit(failed ? 1 : 0);
  }
  // one request through the router, as an agent would send it
  const ask = (model, text, stream = false) => fetch(base + "/v1/chat/completions", { method: "POST",
    headers: { "Content-Type": "application/json", "X-Router-Client": "ui-test" },
    body: JSON.stringify({ model, stream, messages: [{ role: "user", content: text }] }) }).then((r) => r.text());
  return { base, fakeURL, js, until, open, get, check, finish, ask };
}
