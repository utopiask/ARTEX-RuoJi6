const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const { createRequire } = require("node:module");
const req = createRequire(path.join(__dirname, "../../package.json"));
const ts = req("typescript");
const React = req("react");
const { renderToStaticMarkup } = req("react-dom/server");

// 1) 翻译合同：assetTransfer.* 每个键都必须有中文与英文两种非空文案。
const messagesSource = fs.readFileSync(path.join(__dirname, "../i18n/messages.ts"), "utf8");
const compiledMessages = ts.transpileModule(messagesSource, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText;
const messagesExports = {};
vm.runInNewContext(compiledMessages, {
  exports: messagesExports,
  require: (id) => (id.includes("screen-catalog") ? {} : {}),
  console,
});
const messages = messagesExports.messages;

test("assetTransfer i18n keys cover both languages", () => {
  const keys = Object.keys(messages).filter((k) => k.startsWith("assetTransfer."));
  assert.ok(keys.length >= 30, `expected >=30 assetTransfer keys, got ${keys.length}`);
  for (const key of keys) {
    const pair = messages[key];
    assert.ok(Array.isArray(pair) && pair.length === 2, `${key} must be [zh, en]`);
    assert.ok(pair[0].trim() !== "" && pair[1].trim() !== "", `${key} has an empty translation`);
  }
  // Every literal component translation key must be bilingual.
  const component = fs.readFileSync(path.join(__dirname, "../components/asset-transfer.tsx"), "utf8");
  for (const [, key] of component.matchAll(/ui\("(assetTransfer\.[^"]+)"/g)) {
    assert.ok(messages[key], `missing ${key}`);
  }
  // 组件用到的键必须在词典里（抽查硬绑定键）
  for (const key of [
    "assetTransfer.import",
    "assetTransfer.export",
    "assetTransfer.confirmImport",
    "assetTransfer.stale",
  ]) {
    assert.ok(messages[key], `missing ${key}`);
  }
});

// 2) 渲染：导出弹窗在英文环境无中文残留、按钮与敏感提示可见（A22/A17 界面面）。
const filename = path.join(__dirname, "../components/asset-transfer.tsx");
const compiled = ts.transpileModule(fs.readFileSync(filename, "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
}).outputText;

function renderDialog(lang, props = {}) {
  const states = [];
  const hooks = {
    ...React,
    useState(initial) {
      const value = typeof initial === "function" ? initial() : initial;
      states.push(value);
      return [value, () => {}];
    },
    useEffect() {},
  };
  const t = (key, values) => {
    const pair = messages[key];
    let out = pair ? (lang === "en" ? pair[1] : pair[0]) : key;
    if (values) for (const [k, v] of Object.entries(values)) out = out.replaceAll(`{${k}}`, String(v));
    return out;
  };
  const noop = () => undefined;
  const stub = (props2) =>
    React.createElement("stub", props2, ...(Array.isArray(props2.children) ? props2.children : [props2.children]));
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    React: hooks,
    require(id) {
      if (id === "react") return hooks;
      if (id === "react/jsx-runtime") return req("react/jsx-runtime");
      if (id === "@/i18n/use-ui-translation") return { useUITranslation: () => ({ ui: t, liveUI: t }) };
      if (id === "@/lib/api")
        return {
          api: {
            assetExport: noop,
            assetImportTemplate: noop,
            assetImportPreview: noop,
            assetImportBatch: noop,
            assetImportCommit: noop,
            assetImportProbe: noop,
            assetImportErrorsURL: () => null,
          },
          getToken: () => null,
        };
      if (
        id.endsWith("/ui/button") ||
        id.endsWith("/ui/checkbox") ||
        id.endsWith("/ui/dialog") ||
        id.endsWith("/ui/field") ||
        id.endsWith("/ui/input") ||
        id.endsWith("/ui/label") ||
        id.endsWith("/ui/radio-group") ||
        id.endsWith("/ui/scroll-area") ||
        id.endsWith("/ui/select") ||
        id.endsWith("/ui/table") ||
        id.endsWith("/ui/tabs") ||
        id.endsWith("/ui/textarea") ||
        id.endsWith("/ui/badge")
      )
        return new Proxy({}, { get: (_t, name) => (name === "Root" ? stub : stub) });
      if (id === "sonner") return { toast: { success: noop, error: noop, info: noop } };
      if (id === "lucide-react")
        return new Proxy({}, { get: (_t, name) => () => React.createElement("i", { "data-icon": String(name) }) });
      throw new Error(`Unexpected runtime import: ${id}`);
    },
    window: { createElement: noop },
    document: { createElement: () => ({ click: noop, remove: noop, style: {} }) },
    URL: { createObjectURL: () => "blob:", revokeObjectURL: noop },
    Blob: class {},
    setTimeout: noop,
  });
  const { AssetExportDialog } = exports;
  return renderToStaticMarkup(
    React.createElement(AssetExportDialog, {
      open: true,
      onOpenChange: noop,
      selectedIds: [11, 22],
      ...props,
    }),
  );
}

test("export dialog renders bilingual labels without cross-language leaks", () => {
  const en = renderDialog("en");
  assert.doesNotMatch(en, /[\p{Script=Han}]/u, "EN render must not contain Chinese");
  assert.match(en, /Export Assets/);
  assert.match(en, /Selected 2/);
  assert.match(en, /All assets/);
  assert.match(en, /XLSX/);
  const zh = renderDialog("zh");
  assert.match(zh, /导出资产/);
  assert.match(zh, /勾选的 2 条/);
  assert.match(zh, /全库六类资产/);
});

const compileFile = (name) =>
  ts.transpileModule(fs.readFileSync(path.join(__dirname, name), "utf8"), {
    fileName: name,
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;

test("real API preview uses a parseable multipart boundary and preserves JSON/header behavior", async () => {
  const exports = {},
    requests = [];
  vm.runInNewContext(compileFile("api.ts"), {
    exports,
    FormData,
    URLSearchParams,
    Headers,
    window: {},
    localStorage: { getItem: () => "synthetic-token" },
    require: (id) => (id.endsWith("/enabled") ? { MOCK: false } : {}),
    fetch: async (url, init) => {
      const request = new Request("http://offline.test" + url, init);
      requests.push(request);
      return new Response("{}", { status: 200 });
    },
  });
  for (const file of [undefined, new Blob(["example.test"], { type: "text/plain" })]) {
    await exports.api.assetImportPreview({ text: "example.test", file, options: { conflict: "fill", task_id: 11 } });
    const r = requests.at(-1);
    assert.match(r.headers.get("content-type"), /^multipart\/form-data; boundary=.+/);
    assert.equal(r.headers.get("authorization"), "Bearer synthetic-token");
    const parts = await r.formData();
    assert.equal(parts.get("text"), "example.test");
    assert.equal(JSON.parse(parts.get("options")).task_id, 11);
    if (file) assert.equal(await parts.get("file").text(), "example.test");
  }
  await exports.api.assetImportCommit(7, "hash-seven");
  assert.equal(requests.at(-1).headers.get("content-type"), "application/json");
  assert.deepEqual(await requests.at(-1).json(), { plan_hash: "hash-seven" });
  for (const headers of [
    { "content-type": "application/json", "X-Test": "yes" },
    new Headers({ "Content-Type": "bad", "X-Test": "yes" }),
    [
      ["Content-Type", "bad"],
      ["X-Test", "yes"],
    ],
  ]) {
    const body = new FormData();
    body.append("text", "boundary-check");
    await exports.http("/upload", { method: "POST", headers, body });
    assert.equal((await requests.at(-1).formData()).get("text"), "boundary-check");
    assert.equal(requests.at(-1).headers.get("x-test"), "yes");
  }
  await exports.http("/json", {
    method: "POST",
    body: "{}",
    headers: { "content-type": "custom/json", Authorization: "custom-token" },
  });
  assert.equal(requests.at(-1).headers.get("content-type"), "custom/json");
  assert.equal(requests.at(-1).headers.get("authorization"), "custom-token");
});

const tick = () => new Promise((resolve) => setImmediate(resolve));
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((a, b) => {
    resolve = a;
    reject = b;
  });
  return { promise, resolve, reject };
};
const summaryOf = (extra) => ({
  total: 1,
  create: 1,
  fill: 0,
  overwrite: 0,
  unchanged: 0,
  conflict: 0,
  invalid: 0,
  ...extra,
});
const plan = (id, summary = summaryOf()) => ({ batch_id: id, plan_hash: `hash-${id}`, summary });
const details = (id, summary = summaryOf()) => ({
  ...plan(id, summary),
  status: "ready",
  rows: [{ index: 0, action: "create", row: { line: 1, type: "root_domain", domain: `batch-${id}.test` } }],
  rows_total: 1,
  offset: 0,
  limit: 100,
});
function harness(overrides = {}, initialProps = {}) {
  const slots = [],
    effects = [],
    errors = [],
    commits = [],
    previews = [],
    downloads = [];
  let cursor = 0,
    tree,
    props = { open: true, presetTaskId: 11, onOpenChange() {}, onImported() {}, ...initialProps },
    next = 0;
  const hooks = {
    useState(initial) {
      const i = cursor++;
      if (!(i in slots)) slots[i] = typeof initial === "function" ? initial() : initial;
      return [
        slots[i],
        (v) => {
          slots[i] = typeof v === "function" ? v(slots[i]) : v;
        },
      ];
    },
    useRef(initial) {
      const i = cursor++;
      if (!(i in slots)) slots[i] = { current: initial };
      return slots[i];
    },
    useEffect(fn, deps) {
      const i = cursor++;
      const old = slots[i];
      if (!old || deps.some((v, j) => v !== old.deps[j])) {
        effects.push(() => {
          old?.cleanup?.();
          slots[i] = { deps, cleanup: fn() };
        });
      }
    },
  };
  const api = {
    companies: async () => [],
    tasks: async () => ({ tasks: [] }),
    assetImportPreview: async (input) => {
      previews.push(input);
      return plan(++next);
    },
    assetImportBatch: async (id) => details(id),
    assetImportCommit: async (...args) => {
      commits.push(args);
      return { receipt: { ...plan(args[0]), attached: 0 } };
    },
    assetImportProbe: async () => ({ queued: 0, not_queued: 0, skipped: 0 }),
    assetImportErrorsURL: (id) => `/errors/${id}`,
    ...overrides,
  };
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    console,
    require(id) {
      if (id === "react") return hooks;
      if (id === "react/jsx-runtime")
        return { jsx: (type, props) => ({ type, props }), jsxs: (type, props) => ({ type, props }) };
      if (id === "@/lib/api") return { api, getToken: () => "token" };
      if (id === "@/i18n/use-ui-translation") return { useUITranslation: () => ({ ui: (key) => key }) };
      if (id === "sonner") return { toast: { error: (e) => errors.push(e), success() {}, info() {} } };
      return new Proxy({}, { get: (_, name) => String(name) });
    },
    fetch: async (url, init) => {
      downloads.push({ url, init });
      return { ok: true, blob: async () => new Blob(["error"]) };
    },
    URL: { createObjectURL: () => "blob:test", revokeObjectURL() {} },
    document: { createElement: () => ({ click() {}, remove() {} }), body: { appendChild() {} } },
  });
  const nodes = (n) =>
    Array.isArray(n) ? n.flatMap(nodes) : n && typeof n === "object" ? [n, ...nodes(n.props?.children)] : [];
  const render = (changes) => {
    props = { ...props, ...changes };
    cursor = 0;
    tree = exports.AssetImportDialog(props);
    while (effects.length) effects.shift()();
    cursor = 0;
    tree = exports.AssetImportDialog(props);
    return tree;
  };
  const button = (key) =>
    nodes(tree).find((n) => n.type === "Button" && JSON.stringify(n.props.children).includes(key));
  const input = (value) => {
    nodes(tree)
      .find((n) => n.type === "Textarea")
      .props.onChange({ target: { value } });
    render();
  };
  const click = (key) => {
    const b = button(key);
    assert.ok(b, `missing ${key}`);
    if (!b.props.disabled) b.props.onClick();
    render();
  };
  render();
  return { render, nodes: () => nodes(tree), button, input, click, errors, commits, previews, downloads, api };
}
async function preview(h) {
  h.input("example.test");
  h.click("assetTransfer.preview");
  await tick();
  h.render();
}

test("R04 failed second details request cannot expose or commit either old or new batch", async () => {
  const pending = deferred();
  const h = harness({ assetImportBatch: async (id) => (id === 2 ? pending.promise : details(id)) });
  await preview(h);
  const oldCommit = h.button("assetTransfer.confirmImport").props.onClick;
  h.click("assetTransfer.rePreview");
  h.input("second.test");
  h.click("assetTransfer.preview");
  await tick();
  h.render();
  assert.equal(h.button("assetTransfer.confirmImport"), undefined);
  oldCommit();
  await tick();
  assert.equal(h.commits.length, 0);
  pending.reject(Error("GET failed"));
  await tick();
  h.render();
  assert.equal(h.button("assetTransfer.confirmImport"), undefined);
  assert.ok(h.errors.includes("GET failed"));
  assert.ok(!JSON.stringify(h.nodes()).includes("batch-1.test"));
});

test("preview details must match batch ID, hash and ready status", async () => {
  for (const mismatch of [{ batch_id: 2 }, { plan_hash: "different" }, { status: "expired" }]) {
    const h = harness({ assetImportBatch: async (id) => ({ ...details(id), ...mismatch }) });
    await preview(h);
    assert.equal(h.button("assetTransfer.confirmImport"), undefined);
    assert.ok(h.errors.includes("assetTransfer.stale"));
    assert.equal(h.commits.length, 0);
  }
});

test("late preview POST and detail GET responses cannot replace the newest preview", async () => {
  for (const delayedStage of ["post", "get"]) {
    const pending = deferred();
    let count = 0;
    const h = harness({
      assetImportPreview: async () => (++count === 1 && delayedStage === "post" ? pending.promise : plan(count)),
      assetImportBatch: async (id) => (id === 1 && delayedStage === "get" ? pending.promise : details(id)),
    });
    h.input("first.test");
    h.click("assetTransfer.preview");
    await tick();
    h.render();
    h.input("second.test");
    h.click("assetTransfer.preview");
    await tick();
    h.render();
    assert.ok(JSON.stringify(h.nodes()).includes("batch-2.test"));
    pending.resolve(delayedStage === "post" ? plan(1) : details(1));
    await tick();
    h.render();
    assert.ok(JSON.stringify(h.nodes()).includes("batch-2.test"));
    h.click("assetTransfer.confirmImport");
    await tick();
    assert.deepEqual(h.commits[0], [2, "hash-2"]);
  }
});

test("unchanged-only task associations commit; unchanged without a task and invalid-only plans do not", async () => {
  for (const [presetTaskId, counts, allowed] of [
    [11, { create: 0, unchanged: 1 }, true],
    [undefined, { create: 0, unchanged: 1 }, false],
    [11, { create: 0, invalid: 1 }, false],
    [11, { create: 0, conflict: 1 }, false],
  ]) {
    const summary = summaryOf(counts);
    const h = harness(
      { assetImportPreview: async () => plan(1, summary), assetImportBatch: async () => details(1, summary) },
      { presetTaskId },
    );
    await preview(h);
    assert.equal(h.button("assetTransfer.confirmImport").props.disabled, !allowed);
    h.click("assetTransfer.downloadErrors");
    await tick();
    assert.equal(h.downloads[0].url, "/errors/1");
    assert.equal(h.downloads[0].init.headers.Authorization, "Bearer token");
    h.click("assetTransfer.confirmImport");
    await tick();
    assert.equal(h.commits.length, allowed ? 1 : 0);
  }
});

test("preset task changes reset target and preview, and close ignores pending requests", async () => {
  const h = harness();
  await preview(h);
  const stale = h.button("assetTransfer.confirmImport").props.onClick;
  h.render({ presetTaskId: 22 });
  stale();
  await tick();
  assert.equal(h.commits.length, 0);
  await preview(h);
  assert.equal(h.previews.at(-1).options.task_id, 22);
  h.render({ presetTaskId: undefined });
  await preview(h);
  assert.equal(h.previews.at(-1).options.task_id, undefined);
  const pending = deferred();
  h.api.assetImportPreview = () => pending.promise;
  h.click("assetTransfer.rePreview");
  h.click("assetTransfer.preview");
  h.render({ open: false });
  pending.resolve(plan(99));
  await tick();
  h.render({ open: true });
  assert.equal(h.button("assetTransfer.confirmImport"), undefined);
});

test("every input and option change invalidates previously captured commit handlers", async () => {
  for (const field of ["text", "file", "conflict", "company", "policy", "task"]) {
    const h = harness({}, { presetTaskId: undefined });
    if (field === "policy") {
      h.nodes()
        .find((n) => n.type === "Select" && n.props.value === "0")
        .props.onValueChange("5");
      h.render();
    }
    const sourceNodes = h.nodes();
    await preview(h);
    const stale = h.button("assetTransfer.confirmImport").props.onClick;
    if (field === "text" || field === "file") {
      const n = sourceNodes.find((n) => n.type === (field === "text" ? "Textarea" : "Input"));
      n.props.onChange({ target: { value: "changed.test", files: [new Blob(["changed"])] } });
    } else if (field === "conflict") sourceNodes.find((n) => n.type === "RadioGroup").props.onValueChange("overwrite");
    else {
      const selects = sourceNodes.filter((n) => n.type === "Select");
      selects[field === "task" ? selects.length - 1 : field === "policy" ? 1 : 0].props.onValueChange(
        field === "policy" ? "keep" : "7",
      );
    }
    stale();
    await tick();
    h.render();
    assert.equal(h.commits.length, 0, field);
    assert.equal(h.button("assetTransfer.confirmImport"), undefined, field);
  }
});

test("page response races preserve latest rows and failed pages invalidate confirmation", async () => {
  const h = harness();
  await preview(h);
  const first = deferred(),
    second = deferred();
  let n = 0;
  h.api.assetImportBatch = () => (++n === 1 ? first.promise : second.promise);
  const filters = h.nodes().filter((n) => n.type === "button");
  filters[0].props.onClick();
  h.render();
  assert.equal(h.button("assetTransfer.confirmImport").props.disabled, true);
  filters[1].props.onClick();
  h.render();
  const newest = details(1);
  newest.rows[0].row.domain = "latest-page.test";
  second.resolve(newest);
  await tick();
  h.render();
  first.resolve(details(1));
  await tick();
  h.render();
  assert.ok(JSON.stringify(h.nodes()).includes("latest-page.test"));
  h.api.assetImportBatch = async () => {
    throw Error("page failed");
  };
  h.nodes()
    .find((n) => n.type === "button")
    .props.onClick();
  await tick();
  h.render();
  assert.equal(h.button("assetTransfer.confirmImport"), undefined);
  assert.equal(h.commits.length, 0);
});

test("double confirmation dispatches only one commit", async () => {
  const h = harness();
  await preview(h);
  const commit = h.button("assetTransfer.confirmImport").props.onClick;
  commit();
  commit();
  await tick();
  h.render();
  assert.equal(h.commits.length, 1);
});
