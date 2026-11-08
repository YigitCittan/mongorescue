// Loads the dashboard's classic scripts (web/static/*.js) into a node:vm context so
// their pure helpers can be tested without a browser. The scripts share one global
// scope in the browser (top-level function and const declarations of one script are
// visible to the next), which one vm context reproduces. The DOM is a permissive
// stub: every property is another stub, every call returns one, so load-time code
// that touches the document does not throw. Only helpers that do not read the DOM
// are meaningful to test.
import { readFileSync } from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

/** STATIC_DIR is web/static. */
export const STATIC_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "static");

/** readStatic returns a static file with LF line endings. */
export function readStatic(name) {
  return readFileSync(path.join(STATIC_DIR, name), "utf8").replace(/\r\n/g, "\n");
}

// stub returns a callable object that answers every property and call with another stub.
function stub() {
  const fn = function () {};
  const proxy = new Proxy(fn, {
    get(_t, prop) {
      if (prop === Symbol.toPrimitive) return () => "";
      if (prop === "then") return undefined;
      if (prop === "length") return 0;
      if (prop === "matches") return false;
      return stub();
    },
    set() { return true; },
    apply() { return stub(); },
    construct() { return stub(); },
  });
  return proxy;
}

// memoryStorage is a minimal localStorage.
function memoryStorage() {
  const m = new Map();
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: k => { m.delete(k); },
  };
}

/**
 * createContext returns a fresh global scope with the DOM stubbed. Options:
 * `lang` is the navigator language.
 */
export function createContext({ lang = "en-US" } = {}) {
  const localStorage = memoryStorage();
  const document = stub();
  const sandbox = {
    console,
    Intl,
    Date,
    Math,
    JSON,
    Promise,
    URL,
    URLSearchParams,
    setTimeout: () => 0,
    clearTimeout: () => {},
    setInterval: () => 0,
    clearInterval: () => {},
    fetch: () => new Promise(() => {}),
    document,
    localStorage,
    sessionStorage: memoryStorage(),
    navigator: { language: lang, userAgent: "node" },
    location: { hash: "", search: "", pathname: "/", href: "http://localhost/", origin: "http://localhost" },
    history: { pushState() {}, replaceState() {} },
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    Element: class Element {},
    HTMLElement: class HTMLElement {},
    HTMLSelectElement: class HTMLSelectElement {},
    HTMLInputElement: class HTMLInputElement {},
    Node: class Node {},
    MutationObserver: class MutationObserver { observe() {} disconnect() {} },
    ResizeObserver: class ResizeObserver { observe() {} disconnect() {} },
    IntersectionObserver: class IntersectionObserver { observe() {} disconnect() {} },
    requestAnimationFrame: () => 0,
    cancelAnimationFrame: () => {},
    addEventListener() {},
    removeEventListener() {},
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  return vm.createContext(sandbox);
}

/** loadScript runs one static script in ctx, as a classic script. */
export function loadScript(ctx, name) {
  vm.runInContext(readStatic(name), ctx, { filename: name });
}

/** evalIn evaluates an expression in ctx (it sees the scripts' top-level const/let too). */
export function evalIn(ctx, expr) {
  return vm.runInContext(expr, ctx);
}

/** loadScripts loads the scripts in order and returns the context. */
export function loadScripts(names, options) {
  const ctx = createContext(options);
  for (const n of names) loadScript(ctx, n);
  return ctx;
}
