import { parse as parseJavaScript } from "acorn";
import { parse as parseHTML } from "parse5";

const identifier = String.raw`[A-Za-z_$][\w$]*`;

const signerWrapperPattern = new RegExp(
  `async function (${identifier})\\((${identifier}),(${identifier})\\)\\{` +
    `(${identifier})=\\4\\|\\|new Promise\\((${identifier})=>\\{` +
    `(${identifier})\\.A\\((\\d+)\\)\\.then\\((${identifier})=>` +
    `\\5\\(\\8\\.default\\(\\)\\)\\)\\}\\);` +
    `let (${identifier})=await \\4;return await \\9\\(\\2,\\3\\)\\}`,
);

const cachedFactorySignerPattern = new RegExp(
  `let (${identifier})=\\((${identifier})=async\\(\\)=>\\(await (${identifier})\\.A\\((\\d+)\\)\\)\\.default\\(\\),` +
    `async function\\((${identifier}),(${identifier})\\)\\{` +
    `(${identifier})\\?\\?=\\2\\(\\)\\.catch\\((${identifier})=>\\{throw \\7=void 0,\\8\\}\\);` +
    `let (${identifier})=await \\7;return await \\9\\(\\5,\\6\\)\\}\\),` +
    `(${identifier})=`,
);

export function patchStatsigChunk(source) {
  if (
    typeof source !== "string" ||
    !source.includes("x-statsig-id") ||
    source.includes("__grok2apiStatsigSign")
  ) {
    return { patched: false, source };
  }

  const legacyMatch = signerWrapperPattern.exec(source);
  if (legacyMatch) {
    const replacement = `${legacyMatch[0]}globalThis.__grok2apiStatsigSign=${legacyMatch[1]};`;
    return {
      patched: true,
      source:
        source.slice(0, legacyMatch.index) +
        replacement +
        source.slice(legacyMatch.index + legacyMatch[0].length),
      functionName: legacyMatch[1],
      loaderModuleID: legacyMatch[7],
    };
  }

  const factoryMatch = cachedFactorySignerPattern.exec(source);
  if (factoryMatch) {
    const nextVariable = factoryMatch[10];
    const declaration = factoryMatch[0].slice(0, -(`,${nextVariable}=`.length));
    const replacement = `${declaration};globalThis.__grok2apiStatsigSign=${factoryMatch[1]};let ${nextVariable}=`;
    return {
      patched: true,
      source:
        source.slice(0, factoryMatch.index) +
        replacement +
        source.slice(factoryMatch.index + factoryMatch[0].length),
      functionName: factoryMatch[1],
      loaderModuleID: factoryMatch[4],
    };
  }

  const structuralMatch = findStructuralSignerWrapper(source);
  if (!structuralMatch) {
    return { patched: false, source };
  }

  const assignment = "globalThis.__grok2apiStatsigSign=";
  if (structuralMatch.declarationName) {
    const insertionIndex = structuralMatch.end;
    return {
      patched: true,
      source:
        source.slice(0, insertionIndex) +
        `;${assignment}${structuralMatch.declarationName};` +
        source.slice(insertionIndex),
      functionName: structuralMatch.declarationName,
      loaderModuleID: structuralMatch.loaderModuleID,
    };
  }

  return {
    patched: true,
    source:
      source.slice(0, structuralMatch.start) +
      assignment +
      source.slice(structuralMatch.start),
    functionName: structuralMatch.functionName,
    loaderModuleID: structuralMatch.loaderModuleID,
  };
}

function findStructuralSignerWrapper(source) {
  const syntaxTree = parseChunk(source);
  if (!syntaxTree) {
    return undefined;
  }

  const candidates = [];
  walkSyntax(syntaxTree, [], (node, ancestors) => {
    const factoryCandidate = createBotoxSignerCandidate(node);
    if (factoryCandidate) {
      candidates.push(factoryCandidate);
    }

    if (!isAsyncTwoArgumentFunction(node)) {
      return;
    }
    const firstParameter = node.params[0].name;
    const secondParameter = node.params[1].name;
    if (!returnsSignerCall(node.body, firstParameter, secondParameter)) {
      return;
    }

    const contextNode = findWrapperContext(node, ancestors);
    const loader = findSignerLoader(contextNode, node.start);
    if (!loader) {
      return;
    }
    const context = source.slice(contextNode.start, contextNode.end);
    let score = loader.index >= node.start ? 4 : 2;
    if (/\?\?=|\|\|=|\.catch\(|new Promise\(/.test(context)) {
      score += 1;
    }
    if (containsAwaitOutsideNestedFunction(node.body)) {
      score += 1;
    }

    const declarationName = node.type === "FunctionDeclaration" ? node.id?.name ?? "" : "";
    candidates.push({
      start: node.start,
      end: node.end,
      declarationName,
      functionName: declarationName || inferFunctionName(node, ancestors) || "anonymous",
      loaderModuleID: loader.moduleID,
      score,
    });
  });

  candidates.sort((left, right) => right.score - left.score);
  if (candidates.length === 0 || (candidates.length > 1 && candidates[0].score === candidates[1].score)) {
    return undefined;
  }
  return candidates[0];
}

function createBotoxSignerCandidate(node) {
  if (
    node.type !== "VariableDeclarator" ||
    node.id?.type !== "Identifier" ||
    node.init?.type !== "CallExpression" ||
    calledFunctionName(node.init.callee) !== "createBotoxSigner" ||
    node.init.arguments?.length !== 1
  ) {
    return undefined;
  }

  const loaderFactory = node.init.arguments[0];
  if (
    (loaderFactory.type !== "FunctionExpression" && loaderFactory.type !== "ArrowFunctionExpression") ||
    loaderFactory.async !== true ||
    loaderFactory.params?.length !== 0
  ) {
    return undefined;
  }
  const loader = findSignerLoader(loaderFactory, node.init.start);
  if (!loader) {
    return undefined;
  }
  return {
    start: node.init.start,
    end: node.init.end,
    declarationName: "",
    functionName: node.id.name,
    loaderModuleID: loader.moduleID,
    score: 10,
  };
}

function calledFunctionName(node) {
  const value = node?.type === "ChainExpression" ? node.expression : node;
  if (value?.type === "SequenceExpression") {
    return calledFunctionName(value.expressions.at(-1));
  }
  return value?.type === "MemberExpression" ? memberName(value) : undefined;
}

function parseChunk(source) {
  for (const sourceType of ["script", "module"]) {
    try {
      return parseJavaScript(source, {
        ecmaVersion: "latest",
        sourceType,
        allowAwaitOutsideFunction: true,
        allowReturnOutsideFunction: sourceType === "script",
      });
    } catch {
      // Try the other source type before treating this chunk as unsupported.
    }
  }
  return undefined;
}

function isAsyncTwoArgumentFunction(node) {
  return (
    (node.type === "FunctionDeclaration" ||
      node.type === "FunctionExpression" ||
      node.type === "ArrowFunctionExpression") &&
    node.async === true &&
    node.body?.type === "BlockStatement" &&
    node.params?.length === 2 &&
    node.params.every((parameter) => parameter.type === "Identifier")
  );
}

function returnsSignerCall(body, firstParameter, secondParameter) {
  let found = false;
  walkFunctionBody(body, (node) => {
    if (node.type !== "ReturnStatement") {
      return;
    }
    const value = unwrapAwait(node.argument);
    if (
      value?.type === "CallExpression" &&
      value.callee?.type === "Identifier" &&
      value.arguments?.length === 2 &&
      value.arguments[0]?.type === "Identifier" &&
      value.arguments[0].name === firstParameter &&
      value.arguments[1]?.type === "Identifier" &&
      value.arguments[1].name === secondParameter
    ) {
      found = true;
    }
  });
  return found;
}

function containsAwaitOutsideNestedFunction(body) {
  let found = false;
  walkFunctionBody(body, (node) => {
    if (node.type === "AwaitExpression") {
      found = true;
    }
  });
  return found;
}

function unwrapAwait(node) {
  let value = node;
  while (value?.type === "AwaitExpression" || value?.type === "ChainExpression") {
    value = value.argument ?? value.expression;
  }
  return value;
}

function findWrapperContext(node, ancestors) {
  for (let index = ancestors.length - 1; index >= 0; index -= 1) {
    if (ancestors[index].type === "VariableDeclarator") {
      return ancestors[index];
    }
  }
  for (let index = ancestors.length - 1; index >= 0; index -= 1) {
    if (ancestors[index].type === "AssignmentExpression") {
      return ancestors[index];
    }
  }
  return node;
}

function inferFunctionName(node, ancestors) {
  if (node.id?.type === "Identifier") {
    return node.id.name;
  }
  for (let index = ancestors.length - 1; index >= 0; index -= 1) {
    const ancestor = ancestors[index];
    if (ancestor.type === "VariableDeclarator" && ancestor.id?.type === "Identifier") {
      return ancestor.id.name;
    }
    if (ancestor.type === "AssignmentExpression" && ancestor.left?.type === "Identifier") {
      return ancestor.left.name;
    }
  }
  return "";
}

function findSignerLoader(contextNode, wrapperStart) {
  const loaders = [];
  const defaultExports = [];
  walkSyntax(contextNode, [], (node, ancestors) => {
    if (isDefaultExportAccess(node)) {
      defaultExports.push(node);
    }
    const loader = parseLoaderCall(node, ancestors);
    if (loader) {
      loaders.push(loader);
    }
  });

  let nearest;
  for (const loader of loaders) {
    const defaultDistance = defaultExports.reduce(
      (distance, access) => Math.min(distance, Math.abs(access.start - loader.index)),
      Number.POSITIVE_INFINITY,
    );
    if (defaultDistance > 600) {
      continue;
    }
    const wrapperDistance = Math.abs(wrapperStart - loader.index);
    if (!nearest || wrapperDistance < nearest.distance) {
      nearest = { ...loader, distance: wrapperDistance };
    }
  }
  return nearest;
}

function parseLoaderCall(node, ancestors) {
  if (
    node.type !== "CallExpression" ||
    node.callee?.type !== "MemberExpression" ||
    node.callee.object?.type !== "Identifier" ||
    node.arguments?.length !== 1
  ) {
    return undefined;
  }
  const method = memberName(node.callee);
  const moduleID = literalValue(node.arguments[0]);
  if (!method || (typeof moduleID !== "string" && typeof moduleID !== "number")) {
    return undefined;
  }
  const parent = ancestors.at(-1);
  const grandparent = ancestors.at(-2);
  const loadedWithThen =
    parent?.type === "MemberExpression" &&
    parent.object === node &&
    memberName(parent) === "then" &&
    grandparent?.type === "CallExpression" &&
    grandparent.callee === parent;
  const loadedWithAwait = ancestors.some((ancestor) => ancestor.type === "AwaitExpression");
  if (method !== "A" && !loadedWithThen && !loadedWithAwait) {
    return undefined;
  }
  return { index: node.start, moduleID: String(moduleID) };
}

function isDefaultExportAccess(node) {
  return node.type === "MemberExpression" && memberName(node) === "default";
}

function memberName(node) {
  if (!node.computed && node.property?.type === "Identifier") {
    return node.property.name;
  }
  return node.computed ? literalValue(node.property) : undefined;
}

function literalValue(node) {
  return node?.type === "Literal" ? node.value : undefined;
}

function walkFunctionBody(node, visitor) {
  visitor(node);
  for (const child of syntaxChildren(node)) {
    if (
      child !== node &&
      (child.type === "FunctionDeclaration" ||
        child.type === "FunctionExpression" ||
        child.type === "ArrowFunctionExpression")
    ) {
      continue;
    }
    walkFunctionBody(child, visitor);
  }
}

function walkSyntax(node, ancestors, visitor) {
  visitor(node, ancestors);
  const nextAncestors = [...ancestors, node];
  for (const child of syntaxChildren(node)) {
    walkSyntax(child, nextAncestors, visitor);
  }
}

function syntaxChildren(node) {
  const children = [];
  for (const value of Object.values(node)) {
    if (Array.isArray(value)) {
      children.push(...value.filter((entry) => entry && typeof entry.type === "string"));
    } else if (value && typeof value.type === "string") {
      children.push(value);
    }
  }
  return children;
}

export function isValidStatsigID(value) {
  const normalized = String(value ?? "").trim();
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(normalized)) {
    return false;
  }
  try {
    return Buffer.from(normalized, "base64").length === 70;
  } catch {
    return false;
  }
}

export function prepareStatsigDocument(source, metaContent = "") {
  if (typeof source !== "string") {
    return { found: false, source, metaContent: "" };
  }

  const document = parseHTML(source, { sourceCodeLocationInfo: true });
  const stack = [document];
  while (stack.length > 0) {
    const node = stack.pop();
    if (node?.tagName === "meta") {
      const attributes = new Map((node.attrs ?? []).map((attribute) => [attribute.name, attribute.value]));
      if (normalizeMetaName(attributes.get("name")) === "grok-site-verification") {
        const currentContent = String(attributes.get("content") ?? "").trim();
        if (!currentContent) {
          return { found: false, source, metaContent: "" };
        }
        if (!metaContent || metaContent === currentContent) {
          return { found: true, source, metaContent: currentContent };
        }
        const location = node.sourceCodeLocation?.attrs?.content;
        if (!location) {
          throw new Error("Grok verification meta has no source location");
        }
        const replacement = `content="${escapeHTMLAttribute(metaContent)}"`;
        return {
          found: true,
          source: source.slice(0, location.startOffset) + replacement + source.slice(location.endOffset),
          metaContent,
        };
      }
    }
    if (Array.isArray(node?.childNodes)) {
      stack.push(...node.childNodes);
    }
  }
  return { found: false, source, metaContent: "" };
}

function normalizeMetaName(value) {
  return String(value ?? "")
    .trim()
    .toLowerCase()
    .replace(/[‐‑‒–—―]/g, "-");
}

function escapeHTMLAttribute(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&quot;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}
