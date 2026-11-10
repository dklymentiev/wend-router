// UI test of the Pro dashboard: every control, pressed the way a person would, and
// what it changed checked in the page and in the router's API.
//
// It runs in its own world (harness.mjs): fake providers with five models of known
// traits in use, a router in a temporary folder, headless Chrome.
//
//   node --experimental-websocket tests/ui/pro.mjs <router binary> [chrome binary]
//
// Node 22 and later need no flag. Exit code 0 when every check passed.

import { startWorld, sleep, GOOD_KEY } from "./harness.mjs";

const { js, until, open: openPage, get, check, finish, fakeURL, ask } = await startWorld("pro.mjs");
const open = (path) => openPage(path, `typeof status !== "undefined" && status.models?.length && meta?.profiles`);
const profiles = async () => (await get("/api/profiles")).profiles;
// page helpers for this page, defined once per load
const HELPERS = `
  window.matchCount = () => +(($("p-summary").textContent.match(/(\\d+) of/) || [])[1] ?? -1);
  window.takes = () => [...(document.querySelector("#p-takes .tlist")?.querySelectorAll(".tname") || [])].map((e) => e.textContent);`;

try {
  await open("/pro");
  await js(HELPERS);
  check("page loads with the five fake models", await js(`return status.models.length`) === 5);
  check("the header says it is running", /Running, \d+ models? up/.test(await js(`return $("run-dot").parentElement.textContent`)));

  // ---- every setting of the profile panel: what it saves, how many models are left
  const ONE = [
    ["Tool calling: Required", `$$('[data-cap="tools"]').value = "require"; fire($$('[data-cap="tools"]'))`, { require: ["tools"] }, 3],
    ["Tool calling: Excluded", `$$('[data-cap="tools"]').value = "exclude"; fire($$('[data-cap="tools"]'))`, { exclude: ["tools"] }, 2],
    ["Vision: Required", `$$('[data-cap="vision"]').value = "require"; fire($$('[data-cap="vision"]'))`, { require: ["vision"] }, 2],
    ["Vision: Excluded", `$$('[data-cap="vision"]').value = "exclude"; fire($$('[data-cap="vision"]'))`, { exclude: ["vision"] }, 3],
    ["Providers: alpha", `const c = $$('[data-prov="alpha"]'); c.checked = true; fire(c)`, { providers: ["alpha"] }, 3],
    ["Name contains: coder", `$("f-names").value = "coder"; fire($("f-names"), "input")`, { models: ["*coder*"] }, 2],
    ["Good for: long", `const c = $$('[data-domain="long"]'); c.checked = true; fire(c)`, { domains: ["long"] }, 1],
    ["Strength: Tier 1 only", `$$('[data-num="max_tier"]').value = "1"; fire($$('[data-num="max_tier"]'))`, { max_tier: 1 }, 1],
    ["Context window: 128K+", `$$('[data-num="min_context"]').value = "128000"; fire($$('[data-num="min_context"]'))`, { min_context: 128000 }, 3],
    ["First token, unmeasured", `$$('[data-num="max_ttft_ms"]').value = "500"; fire($$('[data-num="max_ttft_ms"]'))`, { max_ttft_ms: 500 }, 0],
    ["Order: Fastest first", `$("f-sort").value = "fastest"; fire($("f-sort"))`, { sort: "fastest" }, 5],
    ["Ask the next after: 3 s", `$$('[data-num="hedge_after_ms"]').value = "3000"; fire($$('[data-num="hedge_after_ms"]'))`, { hedge_after_ms: 3000 }, 5],
    ["Price: up to $0.001", `$("f-cost").value = "0.001"; fire($("f-cost"))`, { max_cost: 0.001 }, 5],
    ["No training on prompts", `$("f-private").checked = true; fire($("f-private"))`, { no_training: true }, 3],
    ["Only pinned models, none pinned", `$("f-onlypin").checked = true; fire($("f-onlypin"))`, { only_pinned: true }, 0],
  ];
  for (const [name, act, want, count] of ONE) {
    await js(`$("p-new").click();`);
    await until(`matchCount() === 5`);
    await js(act);
    const ok = await until(`matchCount() === ${count} && JSON.stringify(draft) === ${JSON.stringify(JSON.stringify(want))}`, 4000);
    check(`setting ${name}: saves ${JSON.stringify(want)}, takes ${count}`, ok,
      `draft ${await js(`return JSON.stringify(draft)`)}, takes ${await js(`return matchCount()`)}`);
    await js(`closeEdit();`);
  }

  // ---- a profile from start to end: create, star and reserve, revert, rename, delete
  await js(`$("p-new").click();`);
  await until(`matchCount() === 5`);
  await js(`$("f-pname").value = "uitest-a"; fire($("f-pname"), "input");
    $$('[data-cap="tools"]').value = "require"; fire($$('[data-cap="tools"]'));`);
  await until(`matchCount() === 3`);
  await js(`press($$('[data-pinset="beta/coder-lite|first"]'));`);
  await js(`press($$('[data-pinset="alpha/coder-big|last"]'));`);
  check("star puts a model first, the arrow puts one last", await until(`takes()[0] === "beta/coder-lite" && takes().at(-1) === "alpha/coder-big"`),
    `takes ${await js(`return JSON.stringify(takes())`)}`);
  await js(`$("t-search").value = "coder"; fire($("t-search"), "input");`);
  check("the find box narrows the list to matching names", await until(`takes().length === 2 && takes().every((n) => n.includes("coder"))`),
    `takes ${await js(`return JSON.stringify(takes())`)}`);
  await js(`$("t-search").value = ""; fire($("t-search"), "input");`);
  await js(`$("p-save").click();`);
  await until(`meta.profiles["uitest-a"]`);
  let ps = await profiles();
  check("Save writes the profile to the router", JSON.stringify(ps["uitest-a"]) === JSON.stringify({ require: ["tools"], pinned: ["beta/coder-lite"], fallback: ["alpha/coder-big"] }),
    JSON.stringify(ps["uitest-a"]));

  await js(`closeEdit(); $("p-search").value = "uitest"; fire($("p-search"), "input");`);
  check("profile search shows only matching profiles", await until(`[...document.querySelectorAll("#plist tr[data-edit]")].map((r) => r.dataset.edit).join() === "uitest-a"`),
    await js(`return [...document.querySelectorAll("#plist tr[data-edit]")].map((r) => r.dataset.edit).join()`));
  await js(`document.querySelector('#plist tr[data-edit="uitest-a"]').click();`);
  check("a row of the list opens its profile", await until(`$("pedit").classList.contains("is-open") && $("f-pname").value === "uitest-a"`));

  await js(`const c = $$('[data-prov="alpha"]'); c.checked = true; fire(c);`);
  await until(`draft.providers`);
  await js(`$("p-revert").click();`);
  check("Revert drops a change that was not saved", await until(`!draft.providers && !$$('[data-prov="alpha"]').checked && matchCount() === 3`));

  await js(`$("f-pname").value = "uitest-b"; fire($("f-pname"), "input"); $("p-save").click();`);
  await until(`meta.profiles["uitest-b"] && !meta.profiles["uitest-a"]`);
  ps = await profiles();
  check("Save under a new name renames it, settings kept", !ps["uitest-a"] && ps["uitest-b"]?.pinned?.[0] === "beta/coder-lite", JSON.stringify(Object.keys(ps).filter((k) => k.startsWith("uitest"))));

  await js(`document.querySelector('[data-help="which"]').dispatchEvent(new MouseEvent("mouseover", { bubbles: true }));`);
  const h0 = await js(`return $("p-fields").offsetHeight`);
  check("a ? shows its help on hover without moving the panel", await js(`return !$("hlp-pop").hidden && $("p-fields").offsetHeight === ${h0}`));
  await js(`document.querySelector('[data-help="which"]').dispatchEvent(new MouseEvent("mouseout", { bubbles: true }));`);
  check("the help goes when the pointer leaves", await js(`return $("hlp-pop").hidden`));

  await js(`$("p-delete").click();`);
  await until(`!meta.profiles["uitest-b"]`);
  check("Delete removes the profile from the router", !(await profiles())["uitest-b"]);
  await js(`closeEdit(); $("p-search").value = ""; fire($("p-search"), "input");`);

  // ---- the Models card
  await js(`$("used").checked = true; fire($("used"));`);
  check("'Only models that have served or failed': none before any check", await until(`document.querySelectorAll("#models tr [data-pick]").length === 0`));
  await js(`$("run-kind").value = "check"; $("run-btn").click();`);
  check("Start turns into Stop while a check runs", await until(`$("run-btn").textContent.trim() === "Stop"`, 3000));
  check("the check ends and says so", await until(`$("run-btn").textContent.trim() === "Start" && $("probe-msg").textContent === "Checked 5 models"`, 15000),
    await js(`return $("probe-msg").textContent`));
  check("after the check all five have served", await until(`document.querySelectorAll("#models tr [data-pick]").length === 5`));
  await js(`$("used").checked = false; fire($("used"));`);

  await js(`$("p-new").click();`);
  await until(`matchCount() === 5`);
  await js(`$$('[data-num="max_ttft_ms"]').value = "500"; fire($$('[data-num="max_ttft_ms"]'));`);
  check("First token limit takes the models measured by the check", await until(`matchCount() === 5`), `takes ${await js(`return matchCount()`)}`);
  await js(`closeEdit(); $("p-new").click();`);
  await until(`matchCount() === 5`);
  await js(`$$('[data-cap="reasoning"]').value = "require"; fire($$('[data-cap="reasoning"]'));`);
  check("Thinking: Required takes the one model that reasoned", await until(`matchCount() === 1`), `takes ${await js(`return matchCount()`)}`);
  await js(`closeEdit();`);

  await js(`$("run-kind").value = "test"; $("run-btn").click();`);
  check("a full test lights the rows being tested", await until(`document.querySelectorAll("#models tr.is-running").length > 0`, 4000));
  const first = await js(`return document.querySelector("#models [data-stop-model]")?.dataset.stopModel`);
  await js(`press(document.querySelector("#models [data-stop-model]"));`);
  check("a row's stop mark stops that model and the run goes on",
    await until(`runNow() && !(runNow().now || []).includes(${JSON.stringify(first)})`, 4000), `first ${first}, now ${await js(`return JSON.stringify(runNow())`)}`);
  await js(`$("run-btn").click();`);
  check("Stop ends the run and says so", await until(`!runNow() && $("probe-msg").textContent.startsWith("Stopped")`, 6000),
    await js(`return $("probe-msg").textContent`));

  await js(`document.querySelector('#models-head th[data-sort="model"], th[data-sort="model"]').click();`);
  await sleep(400);   // the table redraws on the next frame
  const names = await js(`return [...document.querySelectorAll("#models tbody [data-pick]")].map((b) => b.dataset.pick)`);
  const dir1 = await js(`return document.querySelector('th[data-sort="model"]').dataset.dir`);
  check("a header sorts the table by it", !!dir1 && JSON.stringify(names) === JSON.stringify([...names].sort((a, b) => dir1 === "desc" ? b.localeCompare(a) : a.localeCompare(b))),
    `${dir1}: ${JSON.stringify(names)}`);
  await js(`document.querySelector('th[data-sort="model"]').click();`);
  check("pressing it again turns the order round", await js(`return document.querySelector('th[data-sort="model"]').dataset.dir`) !== dir1);

  await js(`const p = document.querySelectorAll("#models [data-pick]"); press(p[0]); press(p[1]);`);
  check("picking models shows the selection bar", await until(`!$("selbar").hidden && $("sel-n").textContent.includes("2")`), await js(`return $("selbar").outerHTML.slice(0, 200)`));
  await js(`$("sel-new").click();`);
  check("New profile from selected: those two, pinned, only them", await until(`$("pedit").classList.contains("is-open") && draft.pinned?.length === 2 && draft.only_pinned && matchCount() === 2`),
    `draft ${await js(`return JSON.stringify(draft)`)}`);
  await js(`closeEdit(); $("sel-clear").click();`);
  check("Clear empties the selection", await until(`$("selbar").hidden`));

  // ---- page furniture: chart range, folding, tabs, mode switch
  await js(`document.querySelector('[data-range="1h"]').click();`);
  check("the chart range switch moves to 1 hour", await js(`return document.querySelector('[data-range="1h"]').classList.contains("is-on")`));
  await js(`[...document.querySelectorAll("main .card")].find((c) => c.querySelector(":scope > h2")?.firstChild.textContent.trim() === "Usage by day").querySelector(".fold").click();`);
  await open("/pro");
  await js(HELPERS);
  const usage = `[...document.querySelectorAll("main .card")].find((c) => c.querySelector(":scope > h2")?.firstChild.textContent.trim() === "Usage by day")`;
  check("a folded card stays folded after a reload", await js(`return ${usage}.classList.contains("is-folded")`));
  check("the chart range is remembered too", await js(`return document.querySelector('[data-range="1h"]').classList.contains("is-on")`));
  await js(`${usage}.querySelector(".fold").click();`);
  check("the chevron opens it again", await js(`return !${usage}.classList.contains("is-folded")`));
  // ---- Providers
  await js(`document.querySelector('[data-tab="providers"]').click();`);
  await until(`document.querySelectorAll("#providers tr.pv-grp").length >= 2`);   // the list loads from the router after the click
  const groups = await js(`return [...document.querySelectorAll("#providers tr.pv-grp td")].map((t) => t.firstChild.textContent.trim())`);
  check("providers are grouped: in use, needs a key", JSON.stringify(groups) === JSON.stringify(["In use", "Needs a key"]), JSON.stringify(groups));
  check("a provider without its key offers Add key", await js(`return !!$$('#providers tr[data-pv="gamma"] [data-pvkey]')`));
  await js(`$("pv-search").value = "alp"; fire($("pv-search"), "input");`);
  check("search narrows the list", await js(`return [...document.querySelectorAll("#providers tr[data-pv]")].map((r) => r.dataset.pv).join() === "alpha"`));
  await js(`$("pv-search").value = ""; fire($("pv-search"), "input");`);
  await js(`$$('#providers tr[data-pv="gamma"] [data-pvkey]').click();`);
  check("Add key opens the provider with the key field ready", await until(`$("pvedit").classList.contains("is-open") && $("pv-title").textContent === "Gamma" && $("pv-key")`));
  await js(`$("pv-addkey").click();`);
  check("an empty key is refused", await until(`$("pv-msg").textContent === "Paste a key first."`));
  await js(`$("pv-key").value = "wrong-key"; $("pv-addkey").click();`);
  check("a key the provider refuses is refused", await until(`$("pv-msg").className === "errmsg" && $("pv-msg").textContent !== "Paste a key first."`), await js(`return $("pv-msg").textContent`));
  await js(`$("pv-key").value = ${JSON.stringify(GOOD_KEY)}; $("pv-label").value = "work"; $("pv-addkey").click();`);
  check("a good key is added, masked, with its label", await until(`$("pv-body").querySelectorAll(".pv-key").length === 1 && $("pv-body").textContent.includes("work") && !$("pv-body").textContent.includes(${JSON.stringify(GOOD_KEY)})`, 10000));
  check("with its key the provider moves to In use", await until(`$$('#providers tr[data-pv="gamma"] [data-pvon]')`, 10000));
  await js(`const c = $$('#pvedit [data-pvon="gamma"]'); c.checked = false; fire(c);`);
  check("the panel's switch turns it off, into Off", await until(`$$('#providers tr[data-pv="gamma"]') && [...document.querySelectorAll("#providers tr")].findIndex((r) => r.dataset.pv === "gamma") > [...document.querySelectorAll("#providers tr.pv-grp")].findIndex((r) => r.textContent.startsWith("Off")) && [...document.querySelectorAll("#providers tr.pv-grp")].some((r) => r.textContent.startsWith("Off"))`), JSON.stringify(await js(`return [...document.querySelectorAll("#providers tr")].map((r) => r.dataset.pv || r.textContent.trim())`)));
  await js(`const c = $$('#providers [data-pvon="gamma"]'); c.checked = true; fire(c);`);
  check("the row's switch turns it on again", await until(`providers.find((p) => p.id === "gamma")?.on`) && (await get("/api/providers")).providers.find((p) => p.id === "gamma").on);
  await js(`$("pv-body").querySelector("[data-delkey]").click();`);
  check("Remove on a key deletes it; the provider needs a key again", await until(`$("pv-body").querySelectorAll(".pv-key").length === 0 && $$('#providers tr[data-pv="gamma"] [data-pvkey]')`, 10000));
  await js(`closePv(); $("pv-new").click();`);
  check("+ Add provider opens the form", await until(`$("pvedit").classList.contains("is-open") && $("pv-title").textContent === "Add a provider"`));
  await js(`$("pv-add").click();`);
  check("the form asks for a name and an address", await until(`$("pv-msg").textContent === "A name and a base URL, please."`));
  await js(`$("pv-id").value = "mylab"; $("pv-url").value = ${JSON.stringify(fakeURL + "/a/v1")}; $("pv-add").click();`);
  check("Add provider adds it and shows it", await until(`$("pv-title").textContent === "mylab" && $$('#providers tr[data-pv="mylab"]')`, 10000), await js(`return $("pv-title").textContent`));
  await js(`$("pv-remove").click();`);
  check("Remove deletes the provider", await until(`!$("pvedit").classList.contains("is-open") && !$$('#providers tr[data-pv="mylab"]')`, 8000));
  await js(`document.querySelector('[data-tab="overview"]').click();`);
  // ---- Pipe: one card per request
  await ask("chat-small", "Sent before the tab opened");
  await js(`document.querySelector('[data-tab="pipe"]').click();`);
  check("the pipe goes live when its tab opens", await until(`$("pipe-state").textContent === "Live"`));
  check("what went through before the tab was opened is shown", await until(`[...document.querySelectorAll(".pcard")].some((c) => c.textContent.includes("Sent before the tab opened"))`, 4000));
  await js(`document.querySelector('[data-tab="overview"]').click();`);
  await js(`document.querySelector('[data-tab="pipe"]').click();`);
  check("opening the tab again does not repeat a request", await until(`$("pipe-state").textContent === "Live"`) && await sleep(500) === undefined &&
    await js(`return [...document.querySelectorAll(".pcard")].filter((c) => c.textContent.includes("Sent before the tab opened")).length === 1 && !/okok/.test([...document.querySelectorAll(".pcard")].find((c) => c.textContent.includes("Sent before the tab opened")).textContent)`));
  await js(`$("pipe-clear").click();`);
  // checks from the steps above may still be going out: they show as cards too, so
  // the client's cards are told apart by having no check or test tag
  await js(`window.clientCards = () => [...document.querySelectorAll(".pcard")].filter((c) => !c.querySelector(".tag"));`);
  await ask("coder-big", "Name three colours.", true);
  check("a request shows as a card: what was asked, the model, the answer put together",
    await until(`(() => { const c = clientCards()[0]; return c && c.textContent.includes("Name three colours.") && c.textContent.includes("coder-big") && /answer\\s*ok/.test(c.textContent) && c.textContent.includes("done"); })()`, 8000),
    await js(`return clientCards()[0]?.textContent.slice(0, 300)`));
  await js(`clientCards()[0].querySelector("[data-raw]").click();`);
  check("Raw opens the lines as they passed", await js(`const p = clientCards()[0].querySelector(".praw"); return !!p && p.textContent.includes("client to router")`));
  await js(`$("pipe-pause").click();`);
  await ask("chat-small", "Held while paused?");
  check("Pause holds new requests back", await until(`$("pipe-pause").textContent.startsWith("Resume (") && !$("pipe-pause").textContent.includes("(0 ")`, 6000) && await js(`return clientCards().length === 1`),
    await js(`return $("pipe-pause").textContent`));
  await js(`$("pipe-pause").click();`);
  check("Resume shows them, newest on top", await until(`clientCards().length === 2 && clientCards()[0].textContent.includes("Held while paused?")`));
  await js(`$("pipe-clear").click();`);
  check("Clear empties the pipe", await js(`return document.querySelectorAll(".pcard").length === 0`));
  await js(`startRun("check", "alpha/coder-big");`);
  check("a check the router sends itself shows too, marked as a check",
    await until(`[...document.querySelectorAll(".pcard")].some((c) => c.querySelector(".tag")?.textContent === "check" && c.textContent.includes("coder-big") && c.textContent.includes("done"))`, 8000),
    await js(`return document.querySelector(".pcard")?.textContent.slice(0, 300)`));
  await js(`$("pipe-clear").click();`);

  // ---- Filter: terms, the switch, Try it
  await js(`document.querySelector('[data-tab="filter"]').click();`);
  await until(`$("flt-list").textContent.includes("No terms yet")`);
  await js(`$("flt-term").value = "Acme Corp"; $("flt-type").value = "ORG"; $("flt-addbtn").click();`);
  check("Add puts a term in the list and saves it", await until(`$("flt-list").textContent.includes("Acme Corp")`) && (await get("/api/filter")).terms.includes("Acme Corp	ORG"));
  await js(`$("flt-term").value = "acme corp"; $("flt-addbtn").click();`);
  check("the same term twice is refused", await until(`$("flt-msg").textContent === "Already in the list"`));
  await js(`$("flt-pastebtn").click(); $("flt-pastetext").value = "John Smith, PERSON\\ndb.example.lan\\tHOST"; $("flt-pasteadd").click();`);
  check("Paste a list adds many, with their types", await until(`$("flt-list").querySelectorAll(".flt-row").length === 3`) && (await get("/api/filter")).terms.includes("db.example.lan	HOST"));
  await js(`$("flt-testbtn").click();`);
  check("Test the filter opens a side panel", await until(`$("flttest").classList.contains("is-open") && document.activeElement === $("flt-trytext")`));
  await js(`$("flt-trytext").value = "Hello John"; $("flt-trybtn").click();`);
  check("Test says when none of the terms is in the text", await until(`$("flt-tryout").textContent.startsWith("None of your 3 terms")`),
    await js(`return $("flt-tryout").textContent`));
  await js(`$("flt-trytext").value = "Ask John Smith at Acme Corp"; $("flt-trybtn").click();`);
  check("Test shows below what the model would get, and how many terms", await until(`$("flt-tryout").textContent.includes("2 terms masked") && $("flt-tryout").textContent.includes("Ask <PERSON_1> at <ORG_1>")`), await js(`return $("flt-tryout").textContent`));
  check("the panel offers auto and the models that answer now", await js(`return $("flt-model").options[0].value === "auto" && [...$("flt-model").options].some((o) => o.value === "alpha/coder-big")`));
  await js(`$("flt-model").value = "alpha/coder-big"; $("flt-trytext").value = "Repeat: Mail John Smith at Acme Corp"; $("flt-askbtn").click();`);
  check("Ask the model shows what it got, what it answered, and what you get back",
    await until(`(() => { const t = $("flt-askout").textContent; return t.includes("alpha/coder-big got") && /answered\\s*Mail <PERSON_1> at <ORG_1>/.test(t) && /get back\\s*Mail John Smith at Acme Corp/.test(t); })()`, 8000),
    await js(`return $("flt-askout").textContent`));
  await js(`$("flt-askbtn").click();`);
  check("while waiting the button turns into Stop and the wait is counted", await until(`$("flt-askbtn").textContent === "Stop" && $("flt-askout").textContent.includes("Waiting for alpha/coder-big")`, 2000));
  await js(`$("flt-askbtn").click();`);
  check("Stop gives up on the answer", await until(`$("flt-askbtn").textContent === "Ask the model" && $("flt-askout").textContent.startsWith("Stopped")`, 3000),
    await js(`return $("flt-askout").textContent`));
  await js(`$("flt-tryclear").click();`);
  check("Clear empties the box and the result", await js(`return $("flt-trytext").value === "" && $("flt-tryout").textContent.startsWith("Paste text and press Test")`));
  await js(`$("flt-tryclip").click();`);
  // headless Chrome may or may not hand over the clipboard; either way the panel must say something
  check("Paste reads the clipboard, or says to press Ctrl+V", await until(`$("flt-tryout").textContent.includes("Ctrl+V") || $("flt-trytext").value !== ""`, 3000),
    await js(`return $("flt-tryout").textContent`));
  await js(`$("flt-testx").click();`);
  check("the panel closes", await until(`!$("flttest").classList.contains("is-open") && !$("veil").classList.contains("is-on")`));
  check("the terms are laid out in columns", await js(`return getComputedStyle($("flt-list")).gridTemplateColumns.split(" ").length >= 2`),
    await js(`return getComputedStyle($("flt-list")).gridTemplateColumns`));
  await js(`$("flt-on").checked = true; fire($("flt-on"));`);
  check("the switch turns the filter on", await until(`$("flt-msg").textContent.startsWith("On")`) && (await get("/api/filter")).enabled);
  await js(`document.querySelector('[data-tab="pipe"]').click();`);
  await until(`$("pipe-state").textContent === "Live"`);
  await ask("coder-big", "Call John Smith at Acme Corp");
  check("the Pipe card shows what the model got, the terms masked, next to what was asked",
    await until(`(() => { const c = clientCards().find((c) => c.textContent.includes("Call John Smith")); return c && /model got\\s*Call <PERSON_1> at <ORG_1>/.test(c.textContent); })()`, 8000),
    await js(`return clientCards()[0]?.textContent.slice(0, 300)`));
  await js(`document.querySelector('#pipe-view [data-view="out"]').click();`);
  check("Outgoing shows one line per request to a model, as the model got it",
    await js(`return $("pipe-tunnel").hidden && !$("pipe-out").hidden && [...document.querySelectorAll("#pipe-out .fo")].some((r) => r.textContent.includes("coder-big") && r.textContent.includes("Call <PERSON_1> at <ORG_1>") && !r.textContent.includes("John Smith"))`),
    await js(`return $("pipe-out").textContent.slice(0, 300)`));
  await js(`document.querySelector('#pipe-view [data-view="cards"]').click();`);
  check("Requests brings the cards back", await js(`return !$("pipe-tunnel").hidden && $("pipe-out").hidden`));
  await js(`document.querySelector('[data-tab="filter"]').click();`);
  await js(`document.querySelector('[data-fltdel]').click();`);
  check("Remove takes a term out and saves", await until(`$("flt-list").querySelectorAll(".flt-row").length === 2`) && !(await get("/api/filter")).terms.includes("Acme Corp"));
  // kinds of values from the catalogue, added in the same list
  check("the type list offers kinds of values from the catalogue", await js(`return [...$("flt-type").querySelectorAll("optgroup option")].some((o) => o.value === "@card" && o.textContent === "Any card number")`));
  await js(`$("flt-type").value = "@card"; fire($("flt-type"));`);
  check("a kind needs no value: the field turns off", await js(`return $("flt-term").disabled && $("flt-term").placeholder.startsWith("Every value")`));
  await js(`$("flt-addbtn").click();`);
  check("Add puts the kind in the list and saves it as @card", await until(`[...document.querySelectorAll("#flt-list .flt-row.is-kind")].some((r) => r.textContent.includes("Any card number"))`) &&
    (await get("/api/filter")).terms.includes("@card\tCARD"));
  await js(`$("flt-type").value = "ORG"; fire($("flt-type"));`);
  check("back to a type, the field comes back", await js(`return !$("flt-term").disabled`));
  await js(`$("flt-testbtn").click(); $("flt-trytext").value = "Charge 4111 1111 1111 1111 for John Smith"; $("flt-trybtn").click();`);
  check("Test masks every value of the kind next to the names", await until(`$("flt-tryout").textContent.includes("Charge <CARD_1> for <PERSON_1>")`), await js(`return $("flt-tryout").textContent`));
  await js(`$("flt-testx").click();`);
  await js(`[...document.querySelectorAll("#flt-list .flt-row")].find((r) => r.textContent.includes("Any card number")).querySelector("[data-fltdel]").click();`);
  check("a kind is removed like a term", await until(`!document.querySelector("#flt-list .flt-row.is-kind")`) && !(await get("/api/filter")).terms.includes("@card"));
  await js(`$("flt-on").checked = false; fire($("flt-on"));`);
  await until(`$("flt-msg").textContent.startsWith("Off")`);
  await js(`document.querySelector('[data-tab="overview"]').click();`);
  for (const tab of ["providers", "filter", "pipe", "overview"]) {
    await js(`document.querySelector('[data-tab="${tab}"]').click();`);
    check(`tab ${tab} shows its page`, await js(`return getComputedStyle($("tab-${tab}")).display !== "none"`));
  }
  await js(`$("to-simple").click();`);
  await sleep(1500);
  check("the Simple switch opens the simple page", (await js(`return location.pathname`)) === "/dashboard");

  // ---- Help: the "?" opens the guide of the tab that is open
  await open("/pro", `document.getElementById("help-btn")`);
  await js(`document.querySelector('[data-tab="filter"]').click(); $$("#help-btn").click();`);
  check("? on Filter opens the filter's guide", await until(`$$("#helpdr")?.classList.contains("is-open") && $$("#help-body").querySelector("h1")?.textContent.startsWith("Filter")`));
  await js(`document.querySelector('[data-tab="providers"]').click(); $$("#help-btn").click();`);
  check("? on Providers opens the providers' guide", await until(`$$("#help-body").querySelector("h1")?.textContent.startsWith("Providers")`));
  await js(`document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));`);
  check("a click outside closes it", await until(`!$$("#helpdr").classList.contains("is-open")`));
} catch (e) {
  check("the test ran to the end", false, e.stack || String(e));
} finally {
  await finish();
}
