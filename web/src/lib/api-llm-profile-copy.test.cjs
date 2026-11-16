const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const ts = require("typescript");

require("ts-node").register({
  skipProject: true,
  transpileOnly: true,
  compilerOptions: { target: "ES2022", module: "CommonJS", moduleResolution: "Node" },
});
const D = require("./mock/data.ts");
const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, "api.ts"), "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText;

function client({ mockHandle, fetch } = {}) {
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    Error,
    URLSearchParams,
    require: (id) => {
      if (id === "@/lib/mock/enabled") return { MOCK: Boolean(mockHandle) };
      if (id === "@/lib/mock/handler") return { mockHandle };
      throw new Error(`Unexpected runtime import: ${id}`);
    },
    fetch:
      fetch ??
      (() => {
        throw new Error("Mock profile tests must stay offline");
      }),
  });
  return exports.api;
}

// Each mock scenario gets its own runtime state; exported fixtures stay untouched.
function mockClient() {
  delete require.cache[require.resolve("./mock/handler.ts")];
  return client(require("./mock/handler.ts"));
}

const fixture = {
  name: "DS",
  format: "openai-responses",
  model: "offline-model",
  base_url: "https://llm.invalid/v1",
  proxy: "http://proxy.invalid:8080",
  rate_per_second: 0,
  rate_per_minute: 0,
  context_window_k: 0,
  thinking_type: "disabled",
  reasoning_effort: "",
  priority: 0,
  pool_exclude: false,
  streaming: false,
  max_tokens: 0,
  max_tokens_field: "",
  session_header_key: "X-Offline-Session",
  retry: {
    connect: { attempts: -1, interval_ms: 0 },
    empty: { attempts: 0, interval_ms: 0 },
    stream: { attempts: 3, interval_ms: 250 },
  },
};

function assertNoKey(profile) {
  for (const field of ["api_key", "APIKey", "apiKey"]) assert.equal(Object.hasOwn(profile, field), false);
}

test("duplicate transport POSTs only an empty object to the encoded source path and preserves id/name", async () => {
  const payload = { id: 42, name: "GLM-2" };
  const calls = [];
  const api = client({
    fetch: async (url, init) => {
      calls.push({ url, init });
      return { ok: true, status: 200, json: async () => payload };
    },
  });
  assert.equal(await api.duplicateLLMProfile("17"), payload);
  assert.equal(calls[0].url, "/api/llm/profiles/17/duplicate");
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].init.body, "{}");
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.deepEqual(JSON.parse(calls[0].init.body), {});
  await api.duplicateLLMProfile("bad/id ?");
  assert.equal(calls[1].url, "/api/llm/profiles/bad%2Fid%20%3F/duplicate");
});

test("duplicate transport propagates server errors and error metadata", async () => {
  for (const [status, error, code] of [
    [404, "LLM profile not found", "profile_missing"],
    [409, "LLM profile name is busy; please retry", "profile_name_busy"],
    [500, "could not duplicate LLM profile", "profile_copy_failed"],
  ]) {
    const api = client({
      fetch: async () => ({
        ok: false,
        status,
        json: async () => ({ error, code, params: { id: 17 } }),
      }),
    });
    await assert.rejects(api.duplicateLLMProfile("17"), (failure) => {
      assert.equal(failure.name, "ApiError");
      assert.equal(failure.message, error);
      assert.equal(failure.code, code);
      assert.equal(failure.params.id, 17);
      return true;
    });
  }
});

test("mock copy preserves every DTO field, nested retries, false/zero values and key hints without changing its source", async () => {
  const originalFixtures = structuredClone(D.llmProfiles);
  const api = mockClient();
  const saved = await api.saveLLMProfile({ ...structuredClone(fixture), api_key: "offline-placeholder-only" });
  const sourceID = String(saved.id);
  await api.activateLLMProfile(sourceID);
  const before = await api.llmProfiles();
  const source = before.find((profile) => profile.id === sourceID);
  assert.equal(source.is_default, true);
  assert.ok(source.api_key_hint);
  assertNoKey(source);

  const result = await api.duplicateLLMProfile(sourceID);
  assert.equal(typeof result.id, "number");
  assert.equal(result.name, "DS-2");
  const after = await api.llmProfiles();
  const copy = after.find((profile) => profile.id === String(result.id));
  assert.match(copy.id, /^[1-9]\d*$/);
  assert.notEqual(copy.id, sourceID);
  assert.deepEqual(copy, { ...source, id: String(result.id), name: "DS-2", is_default: false });
  assert.deepEqual(
    after.find((profile) => profile.id === sourceID),
    source,
  );
  assert.notEqual(copy.retry, source.retry);
  assert.notEqual(copy.retry.connect, source.retry.connect);
  assertNoKey(copy);

  // Mutating a returned list cannot mutate either stored profile.
  copy.retry.connect.attempts = 99;
  assert.deepEqual((await api.llmProfiles()).find((profile) => profile.id === copy.id).retry, source.retry);

  const editedRetry = structuredClone(fixture.retry);
  editedRetry.connect.interval_ms = 123;
  await api.saveLLMProfile({
    id: result.id,
    name: "Edited copy",
    format: "anthropic",
    model: "offline-edited",
    api_key: "",
    retry: editedRetry,
    streaming: false,
    max_tokens: 0,
  });
  const editedList = await api.llmProfiles();
  assert.equal(editedList.length, after.length);
  const edited = editedList.find((profile) => profile.id === String(result.id));
  assert.equal(edited.name, "Edited copy");
  assert.equal(edited.format, "anthropic");
  assert.equal(edited.model, "offline-edited");
  assert.equal(edited.api_key_hint, source.api_key_hint);
  assert.equal(edited.streaming, false);
  assert.equal(edited.max_tokens, 0);
  assert.deepEqual(edited.retry, editedRetry);
  assert.deepEqual(
    editedList.find((profile) => profile.id === sourceID),
    source,
  );
  assertNoKey(edited);

  await api.activateLLMProfile(edited.id);
  assert.deepEqual(
    (await api.llmProfiles()).filter((profile) => profile.is_default).map((profile) => profile.id),
    [edited.id],
  );
  assert.equal((await api.deleteLLMProfile(edited.id)).deleted, result.id);
  const remaining = await api.llmProfiles();
  assert.equal(remaining.length, before.length);
  assert.equal(
    remaining.some((profile) => profile.id === edited.id),
    false,
  );
  assert.equal(remaining.find((profile) => profile.id === sourceID).is_default, false);
  assert.deepEqual(D.llmProfiles, originalFixtures);
  assert.deepEqual(await mockClient().llmProfiles(), originalFixtures);
});

test("mock copies fixture hints without storing or requiring an API key", async () => {
  const api = mockClient();
  const source = (await api.llmProfiles())[0];
  const result = await api.duplicateLLMProfile(source.id);
  const profiles = await api.llmProfiles();
  const copy = profiles.find((profile) => profile.id === String(result.id));
  assert.deepEqual(copy, { ...source, id: String(result.id), name: `${source.name}-2`, is_default: false });
  assert.equal(copy.api_key_hint, source.api_key_hint);
  assertNoKey(copy);
  assert.deepEqual(
    profiles.find((profile) => profile.id === source.id),
    source,
  );
});

test("mock naming increments only positive integer suffixes and skips occupied names repeatedly", async () => {
  const api = mockClient();
  const ds = await api.saveLLMProfile({ ...fixture, name: "DS" });
  await api.saveLLMProfile({ ...fixture, name: "DS-2" });
  assert.equal((await api.duplicateLLMProfile(String(ds.id))).name, "DS-3");
  assert.equal((await api.duplicateLLMProfile(String(ds.id))).name, "DS-4");
  for (const [name, expected] of [
    ["GLM-1", "GLM-2"],
    ["GPT4", "GPT4-2"],
    ["GLM-5.3", "GLM-5.3-2"],
    ["GLM-0", "GLM-0-2"],
    ["GLM-01", "GLM-3"],
    ["GLM-+1", "GLM-+1-2"],
    ["GLM-99999999999999999999", "GLM-100000000000000000000"],
  ]) {
    const source = await api.saveLLMProfile({ ...fixture, name });
    assert.equal((await api.duplicateLLMProfile(String(source.id))).name, expected);
  }
  const profiles = await api.llmProfiles();
  assert.equal(new Set(profiles.map((profile) => profile.id)).size, profiles.length);
  assert.equal(new Set(profiles.map((profile) => profile.name)).size, profiles.length);
});

test("mock rejects missing and malformed source IDs without updates", async () => {
  const api = mockClient();
  const before = await api.llmProfiles();
  for (const id of ["999999", "bad", "0", "-1", "1.5", "1e0", "9007199254740992", "1/duplicate", ""]) {
    await assert.rejects(api.duplicateLLMProfile(id), /LLM 配置.*(?:不存在|无效)/);
    assert.deepEqual(await api.llmProfiles(), before);
  }
  assert.deepEqual(D.llmProfiles, before);
});
