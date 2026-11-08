// Unit tests of the i18n engine: lookup with English fallback, placeholders and the
// deep merge the feature scripts use to add their strings. Run with: node --test web/test/
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { evalIn, loadScripts } from "./harness.mjs";

let ctx;
const call = (expr) => evalIn(ctx, expr);

before(() => {
  // trust.js defines mergeTranslations and merges its own strings; bulk.js and
  // jobspager.js merge theirs: the order of index.html.
  ctx = loadScripts(["i18n.js", "app.js", "trust.js", "bulk.js", "jobspager.js"]);
});

describe("t and tf", () => {
  test("translate a key path in the current language", () => {
    call("currentLang = 'en'");
    assert.equal(call("t('bulk.clear')"), "Clear selection");
  });
  test("fall back to English, then the fallback, then the key", () => {
    call("translations.xx = { only: { here: 'x' } }; currentLang = 'xx'");
    assert.equal(call("t('only.here')"), "x");
    assert.equal(call("t('bulk.clear')"), "Clear selection");
    assert.equal(call("t('no.such.key', 'Fallback')"), "Fallback");
    assert.equal(call("t('no.such.key')"), "no.such.key");
    call("currentLang = 'en'; delete translations.xx");
  });
  test("a path that names a group, not a string, is not a translation", () => {
    assert.equal(call("t('bulk')"), "bulk");
  });
  test("tf fills placeholders and keeps unknown ones", () => {
    assert.equal(call("tf('bulk.selected_n', { n: 3 })"), "3 selected");
    assert.equal(call("tf('bulk.selected_n', {})"), "{n} selected");
    assert.equal(call("tf('bulk.selected_n', { n: 0 })"), "0 selected");
    assert.equal(call("tf('x', { a: 1 }, 'v={a} {b}')"), "v=1 {b}");
  });
  test("tf does not treat inherited properties as variables", () => {
    assert.equal(call("tf('x', {}, '{toString}')"), "{toString}");
  });
});

describe("languages", () => {
  test("setLanguage switches and ignores unknown languages", () => {
    call("setLanguage('de')");
    assert.equal(call("currentLang"), "de");
    call("setLanguage('klingon')");
    assert.equal(call("currentLang"), "de");
    call("setLanguage('en')");
  });
  test("the language is persisted", () => {
    call("setLanguage('fr')");
    assert.equal(call("readStoredLang()"), "fr");
    call("setLanguage('en')");
  });
  test("initI18n prefers the stored language, then the browser's", () => {
    call("localStorage.setItem('mongorescue_lang', 'ja'); initI18n()");
    assert.equal(call("currentLang"), "ja");
    call("localStorage.removeItem('mongorescue_lang'); initI18n()");
    assert.equal(call("currentLang"), "en"); // navigator.language is en-US
    call("localStorage.setItem('mongorescue_lang', 'nope'); initI18n()");
    assert.equal(call("currentLang"), "en");
    call("localStorage.removeItem('mongorescue_lang')");
  });
});

describe("mergeTranslations", () => {
  test("adds missing keys deeply and never replaces existing strings", () => {
    const merged = call(`(() => {
      const dst = { a: { x: "keep" }, b: "str" };
      mergeTranslations(dst, { a: { x: "new", y: "added" }, c: { d: "deep" }, b: "other" });
      return JSON.stringify(dst);
    })()`);
    assert.deepEqual(JSON.parse(merged), { a: { x: "keep", y: "added" }, b: "str", c: { d: "deep" } });
  });
  test("replaces a non-object destination with an object", () => {
    const merged = call(`(() => { const dst = { a: null }; mergeTranslations(dst, { a: { b: "c" } }); return JSON.stringify(dst); })()`);
    assert.deepEqual(JSON.parse(merged), { a: { b: "c" } });
  });
  test("cannot pollute Object.prototype", () => {
    call(`mergeTranslations({}, JSON.parse('{"__proto__": {"polluted": "yes"}, "constructor": {"prototype": {"polluted": "yes"}}}'))`);
    assert.equal(call("({}).polluted"), undefined);
  });
  test("feature scripts' strings are merged into the table", () => {
    assert.equal(call("lookup('en', ['pager', 'jobs_label'])"), "Job pages");
    assert.equal(call("lookup('de', ['pager', 'jobs_label'])"), "Job-Seiten");
    assert.equal(call("lookup('en', ['bulk', 'toolbar'])"), "Bulk actions");
  });
});
