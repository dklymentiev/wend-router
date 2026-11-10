// UI test of the Simple dashboard: every control, pressed the way a person would, and
// what it changed checked in the page and in the router's API.
//
// It runs in its own world (harness.mjs): providers alpha and beta in use, gamma
// waiting for a key, a router in a temporary folder, headless Chrome.
//
//   node --experimental-websocket tests/ui/simple.mjs <router binary> [chrome binary]
//
// Node 22 and later need no flag. Exit code 0 when every check passed.

import { startWorld, GOOD_KEY } from "./harness.mjs";

const { js, until, open: openPage, get, check, finish } = await startWorld("simple.mjs");
const open = (path) => openPage(path, `typeof PROV !== "undefined" && PROV.length && META?.names?.length`);
const provs = async () => Object.fromEntries((await get("/api/providers")).providers.map((p) => [p.id, p]));
const profiles = async () => (await get("/api/profiles")).profiles;
const HELPERS = `
  window.listed = () => [...document.querySelectorAll("#providers .it .sn-row__title")].map((e) => e.textContent);
  window.dlgOpen = (id) => $(id).open;
  window.famRows = () => [...document.querySelectorAll("#families > .sn-row .sn-row__title")].map((e) => e.textContent);`;

try {
  await open("/dashboard");
  await js(HELPERS);
  check("page loads and says it is running", await until(`/^Running, \\d+ models? up$/.test($("run").textContent)`), await js(`return $("run").textContent`));

  // ---- providers
  check("the providers in use are listed", JSON.stringify(await js(`return listed()`)) === JSON.stringify(["Alpha", "Beta"]), JSON.stringify(await js(`return listed()`)));
  check("the one waiting for a key is behind '1 more provider'", await js(`return $("more-prov")?.textContent.includes("1 more provider")`));

  await js(`const c = $$('[data-on="beta"]'); c.checked = false; fire(c);`);
  check("a provider's switch turns it off", await until(`PROV.find((p) => p.id === "beta")?.on === false`) && (await provs()).beta.on === false);
  await js(`const c = $$('[data-on="beta"]'); c.checked = true; fire(c);`);
  check("and on again", await until(`PROV.find((p) => p.id === "beta")?.on === true`) && (await provs()).beta.on === true);

  await js(`$$('[data-key="alpha"]').click();`);
  check("Key opens the key dialog for that provider", await until(`dlgOpen("key-dlg") && $("key-q").textContent === "Alpha key"`), await js(`return $("key-q").textContent`));
  await js(`$("key-cancel").click();`);
  check("Cancel closes it", await until(`!dlgOpen("key-dlg")`));

  await js(`$("add-prov").click();`);
  check("Add provider lists the providers not in use", await until(`$("pdrawer").classList.contains("is-open") && $$('#catalog [data-add="gamma"]')`));
  await js(`$$('#catalog [data-add="gamma"]').click();`);
  check("a provider that needs a key asks for it, with where to get one",
    await until(`dlgOpen("key-dlg") && $("key-q").textContent === "Add a Gamma key" && $("key-link").textContent === "example.com"`), await js(`return $("key-q").textContent`));
  await js(`$("key-ok").click();`);
  check("an empty key is refused", await until(`$("key-msg").textContent === "Paste the key first." && $("key-msg").classList.contains("is-bad")`));
  await js(`$("key-input").value = "wrong-key"; $("key-ok").click();`);
  check("a key the provider refuses is refused, the dialog stays", await until(`$("key-msg").classList.contains("is-bad") && $("key-msg").textContent !== "Paste the key first." && dlgOpen("key-dlg")`),
    await js(`return $("key-msg").textContent`));
  await js(`$("key-input").value = ${JSON.stringify(GOOD_KEY)}; $("key-ok").click();`);
  check("a good key adds the provider and turns it on", await until(`!dlgOpen("key-dlg") && listed().includes("Gamma")`, 10000), JSON.stringify(await js(`return listed()`)));
  const g = (await provs()).gamma;
  check("the router keeps the key, masked", g.on && g.keys?.length === 1 && !JSON.stringify(g).includes(GOOD_KEY), JSON.stringify(g.keys));
  await until(`META.families?.some((f) => f.id === "gamma-only")`, 10000);

  // ---- the profile builder
  await js(`$("new-profile").click();`);
  check("New profile opens the builder, Save waits for a model and a name",
    await until(`$("drawer").classList.contains("is-open") && $("dr-title").textContent === "New profile" && $("p-save").disabled && $("p-count").textContent === "Pick at least one model"`));
  await js(`$("p-search").value = "coder"; fire($("p-search"), "input");`);
  check("search narrows the models", JSON.stringify(await js(`return famRows()`)) === JSON.stringify(["coder-big", "coder-lite"]), JSON.stringify(await js(`return famRows()`)));
  await js(`const c = $$('#families input[data-fam="coder-lite"]:not([data-at])'); c.checked = true; fire(c);`);
  check("a model's switch takes it from every provider", await until(`$("p-count").textContent === "2 models at 2 providers"`), await js(`return $("p-count").textContent`));
  await js(`$$('#families [data-unfold="coder-lite"]').click();`);
  await js(`const c = $$('#families input[data-fam="coder-lite"][data-at="beta"]'); c.checked = false; fire(c);`);
  check("unfolded, one provider can be left out", await until(`$("p-count").textContent === "1 model at 1 provider"`), await js(`return $("p-count").textContent`));
  await js(`$("p-name").value = "ui-simple"; fire($("p-name"), "input");`);
  check("with a name, Save is ready", await until(`!$("p-save").disabled`));
  await js(`$("p-save").click();`);
  await until(`META.profiles["ui-simple"]`);
  const saved = (await profiles())["ui-simple"];
  check("Save writes the model, its provider, tools and free only",
    JSON.stringify(saved) === JSON.stringify({ require: ["tools"], free_only: true, families: { "coder-lite": ["gamma"] } }), JSON.stringify(saved));

  await js(`$("p-search").value = ""; $$('[data-edit="ui-simple"]') || [...document.querySelectorAll(".showall")].find((b) => b.textContent.includes("profile"))?.click();`);
  await until(`$$('[data-edit="ui-simple"]')`);
  await js(`$$('[data-edit="ui-simple"]').click();`);
  check("Edit opens the profile as saved",
    await until(`$("dr-title").textContent === "Edit ui-simple" && $$('#families input[data-fam="coder-lite"][data-at="gamma"]')?.checked && !$$('#families input[data-fam="coder-lite"][data-at="beta"]')?.checked`));
  await js(`$("p-delete").click();`);
  check("Delete asks first", await until(`dlgOpen("confirm") && $("cf-q").textContent === "Delete ui-simple?"`));
  await js(`$("cf-ok").click();`);
  check("and then deletes it", await until(`!META.profiles["ui-simple"]`) && !(await profiles())["ui-simple"]);
  await js(`closeDrawers();`);

  // ---- the profile list and Connect
  const names = await js(`return META.names`);
  check("the list shows four profiles and the rest behind a row", await js(`return [...document.querySelectorAll("#profiles > .it")].filter((r) => !r.hidden).length === 4 && !!document.querySelector("#profiles .showall")`));
  await js(`document.querySelector("#profiles .showall").click();`);
  check("that row opens all of them in the side panel", await until(`$("ldrawer").classList.contains("is-open") && $("l-list").querySelectorAll(".it").length === ${names.length}`));
  await js(`closeDrawers();`);
  const second = names[1];
  await js(`$$('#profiles [data-use="${second}"]').click();`);
  check("Use picks the profile in Connect and in the settings", await until(`$("use-model").value === "${second}" && document.querySelector('[data-tpl="any"]').textContent.includes("Model      ${second}")`));
  await js(`$$('[data-how="python"]').click();`);
  check("the Python tab shows its code with that model, the others hidden",
    await js(`return !$$('[data-panel="python"]').hidden && $$('[data-panel="any"]').hidden && $$('[data-tpl="python"]').textContent.includes('model="${second}"')`));
  await js(`$("use-model").value = "${names[0]}"; fire($("use-model"));`);
  check("the model list changes every snippet", await js(`return [...document.querySelectorAll("[data-tpl]")].every((p) => p.textContent.includes("${names[0]}"))`));
  await js(`$$('[data-how="any"]').click(); $("test").click();`);
  check("Test sends a request and says who answered", await until(`$("cnote").textContent.startsWith("It works:")`, 15000), await js(`return $("cnote").textContent`));
  check("the test shows up among the clients", await until(`$("clients").textContent.includes("dashboard-test")`, 10000), await js(`return $("clients").textContent.slice(0, 200)`));
  check("and among the recent requests", await until(`$("recent").querySelectorAll(".sn-row, .it").length > 0`));

  // ---- removing a provider
  await js(`$$('[data-key="gamma"]').click();`);
  await until(`dlgOpen("key-dlg")`);
  await js(`$("key-remove").click();`);
  check("Remove asks first", await until(`dlgOpen("confirm") && $("cf-q").textContent === "Remove Gamma?"`));
  await js(`$("cf-ok").click();`);
  check("and the provider goes back to the catalog, its key deleted",
    await until(`!listed().includes("Gamma")`) && !(await provs()).gamma.listed && !(await provs()).gamma.keys?.length);

  await js(`$("to-pro").click();`);
  await until(`location.pathname === "/pro"`);
  check("the Pro switch opens the pro page", await js(`return location.pathname`) === "/pro");

  // ---- Help: the "?" beside the name opens the guide, Esc closes it
  await js(`localStorage.removeItem("router-mode")`); // the Pro button above made Pro the default
  await open("/dashboard", `document.getElementById("help-btn")`);
  await js(`$$("#help-btn").click();`);
  check("? opens the getting-started guide", await until(`$$("#helpdr")?.classList.contains("is-open") && $$("#help-body").querySelector("h1")?.textContent === "Wend router: getting started"`));
  check("the guide's commands name this router", await js(`return $$("#help-body").textContent.includes(location.origin + "/v1")`));
  check("it offers a copy for an agent", await js(`return $$("#help-copy").textContent === "Copy for your agent"`));
  await js(`document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));`);
  check("Esc closes it", await until(`!$$("#helpdr").classList.contains("is-open")`));
} catch (e) {
  check("the test ran to the end", false, e.stack || String(e));
} finally {
  await finish();
}
