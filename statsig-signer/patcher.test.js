import test from "node:test";
import assert from "node:assert/strict";
import { isValidStatsigID, patchStatsigChunk, prepareStatsigDocument } from "./patcher.js";

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
