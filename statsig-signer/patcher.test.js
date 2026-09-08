import test from "node:test";
import assert from "node:assert/strict";
import {
  inspectStatsigChunk,
  isValidStatsigID,
  patchStatsigChunk,
  prepareStatsigDocument,
} from "./patcher.js";

test("patchStatsigChunk exposes the current Turbopack wrapper", () => {
  const source =
    'const marker="x-statsig-id";async function dY(n,i){t=t||new Promise(t=>{e.A(4629918).then(e=>t(e.default()))});let o=await t;return await o(n,i)}e.s([],6224142);';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "dY");
  assert.equal(result.loaderModuleID, "4629918");
  assert.match(result.source, /globalThis\.__grok2apiStatsigSign=dY;/);
});

test("patchStatsigChunk exposes the cached async factory wrapper", () => {
  const source =
    'const marker="x-statsig-id";let ur=(a=async()=>(await e.A(4629918)).default(),async function(e,t){n??=a().catch(e=>{throw n=void 0,e});let i=await n;return await i(e,t)}),ul=async e=>e;';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "ur");
  assert.equal(result.loaderModuleID, "4629918");
  assert.match(
    result.source,
    /let ur=.*;globalThis\.__grok2apiStatsigSign=ur;let ul=async e=>e;/,
  );
});

test("patchStatsigChunk structurally exposes a changed async function wrapper", () => {
  const source =
    'const marker="x-statsig-id";async function sign(path, method) { cached ||= runtime.load("signer-v2").then(module => module.default()); const signer = await cached; return signer(path, method) };';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "sign");
  assert.equal(result.loaderModuleID, "signer-v2");
  assert.match(result.source, /};globalThis\.__grok2apiStatsigSign=sign;;/);
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk structurally exposes a changed async arrow wrapper", () => {
  const source =
    'const marker="x-statsig-id";const sign=(load=async()=>{let module=await runtime.A("signer-v3");return module["default"]},async(path,method)=>{cache??=load();const signer=await cache;return await signer(path,method)}),next=1;';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "sign");
  assert.equal(result.loaderModuleID, "signer-v3");
  assert.match(result.source, /globalThis\.__grok2apiStatsigSign=async\(path,method\)=>/);
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk structurally exposes a changed anonymous function expression", () => {
  const source =
    'const marker="x-statsig-id";const sign=(load=async()=>{const module=await runtime.A(9123);return module.default},async function(path,method){cache||=load();const signer=await cache;return signer(path,method)}),next=1;';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "sign");
  assert.equal(result.loaderModuleID, "9123");
  assert.match(result.source, /globalThis\.__grok2apiStatsigSign=async function\(path,method\)/);
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk exposes the current createBotoxSigner factory result", () => {
  const source =
    'e.s(["createBotoxSigner",0,function(e){let t;return async function(n,i){t??=e();let o=await t;return await o(n,i)}}],901317);e.s([],6224142);let ac=(0,d.createBotoxSigner)(async()=>(await e.A(4629918)).default()),ap=async e=>{e.headers["x-statsig-id"]=await ac(e.path,e.method)};';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "ac");
  assert.equal(result.loaderModuleID, "4629918");
  assert.match(
    result.source,
    /let ac=globalThis\.__grok2apiStatsigSign=\(0,d\.createBotoxSigner\)/,
  );
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk captures a signer directly from the header usage", () => {
  const source =
    'async function apply(request){request.headers["x-statsig-id"]=await importedSigner(request.path,request.method)}';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "importedSigner");
  assert.equal(result.loaderModuleID, "direct");
  assert.match(
    result.source,
    /await \(globalThis\.__grok2apiStatsigSign=importedSigner,importedSigner\(request\.path,request\.method\)\)/,
  );
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk captures a signer used through Headers.set", () => {
  const source =
    'async function apply(request){request.headers.set("x-statsig-id",await importedSigner(request.path,request.method))}';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "importedSigner");
  assert.equal(result.loaderModuleID, "direct");
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk captures a namespaced signer call", () => {
  const source =
    'async function apply(request){request.headers.set("x-statsig-id",await signerModule.sign(request.path,request.method))}';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "sign");
  assert.match(result.source, /__grok2apiStatsigSign=\(\.\.\.__grok2apiStatsigArgs\)=>\(signerModule\.sign\)/);
  assert.doesNotThrow(() => new Function(result.source));
});

test("patchStatsigChunk preserves an indirect sequence callee", async () => {
  const source =
    'return async function apply(request){request.headers.set("x-statsig-id",await (0,signerModule.sign)(request.path,request.method))}';
  const result = patchStatsigChunk(source);
  const headers = new Map();
  const apply = new Function("signerModule", result.source)({
    sign: async (path, method) => `${method}:${path}`,
  });

  await apply({ headers, path: "/rest/test", method: "POST" });
  assert.equal(headers.get("x-statsig-id"), "POST:/rest/test");
  assert.equal(await globalThis.__grok2apiStatsigSign("/rest/next", "GET"), "GET:/rest/next");
  delete globalThis.__grok2apiStatsigSign;
});

test("patchStatsigChunk captures optional signer calls once", () => {
  for (const call of ["signer?.(request.path,request.method)", "module?.sign(request.path,request.method)"]) {
    const source =
      `async function apply(request){request.headers.set("x-statsig-id",await ${call})}`;
    const result = patchStatsigChunk(source);
    assert.equal(result.patched, true);
    assert.doesNotThrow(() => new Function(result.source));
  }
});

test("patchStatsigChunk follows a signature through a local variable", () => {
  const source =
    'async function apply(request){const id=await signer(request.path,request.method);request.headers.set("x-statsig-id",id)}';
  const result = patchStatsigChunk(source);

  assert.equal(result.patched, true);
  assert.equal(result.functionName, "signer");
  assert.doesNotThrow(() => new Function(result.source));
});

test("inspectStatsigChunk returns bounded static diagnostics", () => {
  const result = inspectStatsigChunk('void "X-Statsig-Id";'.repeat(10));
  assert.equal(result.parseable, true);
  assert.equal(result.exactHeader, true);
  assert.equal(result.statsigMentions, 10);
  assert.equal(result.snippets.length, 3);
  assert.ok(result.snippets.every((snippet) => snippet.length <= 480));
});

test("patchStatsigChunk rejects ambiguous structural wrappers", () => {
  const source =
    'const marker="x-statsig-id";async function first(path,method){let module=await runtime.A(1),signer=module.default;return signer(path,method)}async function second(path,method){let module=await runtime.A(2),signer=module.default;return signer(path,method)}';

  assert.deepEqual(patchStatsigChunk(source), { patched: false, source });
});

test("patchStatsigChunk rejects async two-argument decoys", () => {
  const source =
    'const marker="x-statsig-id";async function request(path,method){return fetch(path,method)}';

  assert.deepEqual(patchStatsigChunk(source), { patched: false, source });
});

test("patchStatsigChunk tolerates helper-separated loader access", () => {
  const filler = "x".repeat(900);
  const source =
    `const marker="x-statsig-id";async function sign(path,method){const module=await runtime.A(77);const ${filler}=0;const signer=module.default;return await signer?.(path,method)}`;
  const result = patchStatsigChunk(source);
  assert.equal(result.patched, true);
  assert.equal(result.functionName, "sign");
  assert.equal(result.loaderModuleID, "77");
});

test("patchStatsigChunk leaves unrelated chunks unchanged", () => {
  const source = 'const header="x-statsig-id";';
  assert.deepEqual(patchStatsigChunk(source), { patched: false, source });
});

test("validates decoded Statsig payload length", () => {
  assert.equal(isValidStatsigID(Buffer.alloc(70).toString("base64").replace(/=+$/, "")), true);
  assert.equal(isValidStatsigID(Buffer.alloc(69).toString("base64")), false);
  assert.equal(isValidStatsigID("not base64"), false);
});

test("replaces the Grok verification meta without rewriting the document", () => {
  const source = '<html><head><meta content="old-value" name="grok-site-verification"><title>Grok</title></head></html>';
  const result = prepareStatsigDocument(source, 'new&"<>value');

  assert.equal(result.found, true);
  assert.equal(result.metaContent, 'new&"<>value');
  assert.equal(
    result.source,
    '<html><head><meta content="new&amp;&quot;&lt;&gt;value" name="grok-site-verification"><title>Grok</title></head></html>',
  );
});

test("reads unicode-hyphen verification meta without changing the source", () => {
  const source = '<meta NAME="grok‑site‑verification" CONTENT="current-value">';
  assert.deepEqual(prepareStatsigDocument(source), {
    found: true,
    source,
    metaContent: "current-value",
  });
});
test("rejects documents without a usable verification meta", () => {
  const source = '<html><head><meta name="description" content="Grok"></head></html>';
  assert.deepEqual(prepareStatsigDocument(source, "new-value"), {
    found: false,
    source,
    metaContent: "",
  });
});
