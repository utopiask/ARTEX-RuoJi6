const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const { createRequire } = require("node:module");
const req = createRequire(path.join(__dirname, "../../package.json"));
const ts = req("typescript");
const React = req("react");
const filename = path.join(__dirname, "../app/(main)/system/llm/page.tsx");
const compiled = ts.transpileModule(fs.readFileSync(filename, "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
}).outputText;

// Run the real page event closures with isolated hook state. These checks do not
// simulate browser layout or pointer hit testing and never contact a backend.
function page(overrides = {}) {
  const source = { id: "7", name: "Synthetic source", format: "openai", model: "fixture-model", api_key_hint: "…test", rate_per_second: 0, rate_per_minute: 0, is_default: false };
  const states = [];
  let stateIndex = 0;
  const notices = [];
  const calls = { copy: [], remove: [], activate: [], list: 0 };
  const api = {
    async duplicateLLMProfile(id) { calls.copy.push(id); return { id: 8, name: "Synthetic source-2" }; },
    async llmProfiles() { calls.list++; return [source]; },
    async llmPool() { return { enabled: false, chain: [] }; },
    async deleteLLMProfile(id) { calls.remove.push(id); },
    async activateLLMProfile(id) { calls.activate.push(id); },
    ...overrides,
  };
  const hooks = {
    ...React,
    useState(initial) {
      const index = stateIndex++;
      states[index] = index === 0 ? [source] : typeof initial === "function" ? initial() : initial;
      return [states[index], (next) => { states[index] = typeof next === "function" ? next(states[index]) : next; }];
    },
    useRef(value) { return { current: value }; },
    useCallback(fn) { return fn; },
    useMemo(fn) { return fn(); },
    useEffect() {},
  };
  const ui = (key, values) => key + (values ? ":" + JSON.stringify(values) : "");
  const stub = (props) => React.createElement("div", props, props.children);
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    require(id) {
      if (id === "react") return hooks;
      if (id === "@/lib/api") return { api };
      if (id === "@/lib/utils") return { cn: (...args) => args.filter(Boolean).join(" ") };
      if (id === "@/i18n/use-ui-translation") return { useUITranslation: () => ({ ui }) };
      if (id === "sonner") return { toast: Object.fromEntries(["success", "warning", "error"].map((kind) => [kind, (text) => notices.push({ kind, text })])) };
      if (id.startsWith("@/components/") || id === "./_components/retry") return new Proxy({}, { get: (_, key) => key === "ZERO_OVERRIDE" ? {} : stub });
      return req(id);
    },
    Map,
    Set,
    console,
  }, { filename });
  const tree = exports.default();
  function nodes(value) {
    if (Array.isArray(value)) return value.flatMap(nodes);
    return value && typeof value === "object" && value.props ? [value, ...nodes(value.props.children)] : [];
  }
  const all = nodes(tree);
  return {
    source, states, notices, calls,
    copy: all.find((node) => node.props["aria-label"] === "llmProfile.copy"),
    remove: all.find((node) => node.props["aria-label"] === "screen.49b82b04bd1a7d"),
    activate: all.find((node) => node.props.className === "flex-1" && typeof node.props.onClick === "function"),
    card: all.find((node) => node.props.role === "button" && typeof node.props.onKeyDown === "function"),
  };
}
const tick = () => new Promise((resolve) => setImmediate(resolve));
const event = () => ({ stopped: false, stopPropagation() { this.stopped = true; } });

test("copy click stops card editing and refreshes only after successful creation", async () => {
  const p = page();
  assert.ok(p.copy);
  const e = event();
  p.copy.props.onClick(e);
  await tick();
  assert.equal(e.stopped, true);
  assert.deepEqual(p.calls.copy, ["7"]);
  assert.equal(p.calls.list, 1);
  assert.equal(p.states[3], false);
  assert.equal(p.states[4], null);
  assert.equal(p.states[5].size, 0);
  assert.equal(p.notices[0].kind, "success");
  assert.match(p.notices[0].text, /Synthetic source-2/);
});

test("the pending ref rejects double clicks and conflicting delete or activate actions", async () => {
  let finish;
  let copies = 0;
  const p = page({ duplicateLLMProfile() { copies++; return new Promise((resolve) => { finish = resolve; }); } });
  p.copy.props.onClick(event());
  p.copy.props.onClick(event());
  p.remove.props.onClick(event());
  p.activate.props.onClick(event());
  assert.equal(copies, 1);
  assert.deepEqual(p.calls.remove, []);
  assert.deepEqual(p.calls.activate, []);
  assert.equal(p.states[5].has("7"), true);
  finish({ id: 8, name: "Synthetic source-2" });
  await tick();
  assert.equal(p.states[5].size, 0);
});

test("refresh failure reports the saved copy instead of inviting duplicate creation", async () => {
  const p = page({ async llmProfiles() { throw new Error("fixture refresh unavailable"); } });
  p.copy.props.onClick(event());
  await tick();
  assert.deepEqual(p.calls.copy, ["7"]);
  assert.equal(p.notices.length, 1);
  assert.equal(p.notices[0].kind, "warning");
  assert.match(p.notices[0].text, /llmProfile.copyRefreshFailed/);
  assert.match(p.notices[0].text, /Synthetic source-2/);
  assert.equal(p.states[5].size, 0);
});

test("request failures release the pending guard for an intentional retry", async () => {
  let attempts = 0;
  const p = page({ async duplicateLLMProfile() { if (++attempts === 1) throw new Error("fixture copy refused"); return { id: 8, name: "Synthetic source-2" }; } });
  p.copy.props.onClick(event());
  await tick();
  assert.equal(p.notices[0].kind, "error");
  assert.equal(p.states[5].size, 0);
  p.copy.props.onClick(event());
  await tick();
  assert.equal(attempts, 2);
  assert.equal(p.notices[1].kind, "success");
});

test("keyboard events from nested card actions do not open the editor", () => {
  const p = page();
  const root = {};
  let prevented = false;
  p.card.props.onKeyDown({ key: "Enter", target: {}, currentTarget: root, preventDefault() { prevented = true; } });
  assert.equal(prevented, false);
  assert.equal(p.states[3], false);
  p.card.props.onKeyDown({ key: "Enter", target: root, currentTarget: root, preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(p.states[3], true);
});
