// Script load order of index.html. The dashboard's scripts are classic scripts that
// share one global scope, so a script may only use another's top-level names after
// that one loaded. This test (1) loads the scripts in the order of index.html into a
// stubbed scope and fails on any load-time ReferenceError, (2) checks that every
// "Loaded after X" / "before Y" statement in a script's header comment holds, and
// (3) checks that every script file is included exactly once.
// Run with: node --test web/test/
import assert from "node:assert/strict";
import { readdirSync } from "node:fs";
import { describe, test } from "node:test";
import { STATIC_DIR, createContext, loadScript, readStatic } from "./harness.mjs";

// scriptOrder returns the names of the local scripts of index.html, in order.
function scriptOrder() {
  const html = readStatic("index.html");
  return [...html.matchAll(/<script\b[^>]*\bsrc="\/([^"/]+\.js)"[^>]*>/g)].map((m) => m[1]);
}

// headerComment returns the first block comment of a script, whitespace collapsed.
function headerComment(name) {
  const m = /^\s*\/\*\*?([\s\S]*?)\*\//.exec(readStatic(name));
  return m ? m[1].replace(/\s*\n\s*\*?\s*/g, " ") : "";
}

// clauseScripts returns the .js names of a clause, up to the end of its sentence.
function clauseScripts(clause) {
  const end = clause.search(/;|\.\s|, whose|, before|, which|, and uses/);
  const part = end < 0 ? clause : clause.slice(0, end);
  return [...part.matchAll(/\b([a-z0-9]+\.js)\b/g)].map((m) => m[1]);
}

const order = scriptOrder();

describe("index.html scripts", () => {
  test("lists every static script exactly once", () => {
    const files = readdirSync(STATIC_DIR).filter((f) => f.endsWith(".js")).sort();
    assert.deepEqual([...order].sort(), files);
  });
  test("starts with i18n.js and app.js", () => {
    assert.deepEqual(order.slice(0, 2), ["i18n.js", "app.js"]);
  });
  test("loads without a ReferenceError, in order", () => {
    const ctx = createContext();
    for (const name of order) {
      assert.doesNotThrow(() => loadScript(ctx, name), `${name} failed at load time`);
    }
  });
});

describe("header comments agree with the load order", () => {
  const index = (n) => order.indexOf(n);
  test("the header comments are still parsed (guards against a changed phrasing)", () => {
    const declared = order.filter((n) => /Loaded after /.test(headerComment(n)));
    assert.ok(declared.length >= 20, `only ${declared.length} scripts declare their dependencies`);
  });
  for (const name of order) {
    const header = headerComment(name);
    const after = /Loaded after (.*)/.exec(header);
    const before = /\bbefore ([a-z0-9]+\.js)/.exec(header);
    if (!after && !before) continue;
    test(name, () => {
      if (after) {
        for (const dep of clauseScripts(after[1])) {
          assert.ok(index(dep) >= 0, `${name} names ${dep}, which index.html does not load`);
          assert.ok(index(dep) < index(name), `${dep} must load before ${name}`);
        }
      }
      if (before) {
        assert.ok(index(name) < index(before[1]), `${name} must load before ${before[1]}`);
      }
    });
  }
});
