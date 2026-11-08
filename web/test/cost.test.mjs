// Unit tests of the storage usage and cost card (cost.js): the strings of the eight
// languages, the formatting and series helpers, the chart and the price payload of
// the storage target form. Run with: node --test web/test/
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { evalIn, loadScripts } from "./harness.mjs";

let ctx;
const call = (expr) => evalIn(ctx, expr);

before(() => {
  // The order of index.html: cost.js merges its strings with trust.js's
  // mergeTranslations and draws with the ov* helpers of overview.js.
  ctx = loadScripts(["i18n.js", "app.js", "trust.js", "overview.js", "cost.js"]);
});

// leaves flattens an object of strings to {"a.b": "text"}.
function leaves(obj, prefix = "") {
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    if (typeof v === "object" && v !== null) Object.assign(out, leaves(v, `${prefix}${k}.`));
    else out[`${prefix}${k}`] = v;
  }
  return out;
}

const placeholders = (s) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();

describe("translations", () => {
  test("every language has the keys and placeholders of English", () => {
    const all = JSON.parse(call("JSON.stringify(COST_TRANSLATIONS)"));
    assert.deepEqual(Object.keys(all).sort(), ["de", "en", "es", "fr", "ja", "ru", "tr", "zh"]);
    const en = leaves(all.en);
    assert.ok(Object.keys(en).length > 20);
    for (const lang of Object.keys(all)) {
      const l = leaves(all[lang]);
      assert.deepEqual(Object.keys(l).sort(), Object.keys(en).sort(), `${lang} keys`);
      for (const key of Object.keys(en)) {
        assert.ok(l[key].trim() !== "", `${lang}.${key} is empty`);
        assert.deepEqual(placeholders(l[key]), placeholders(en[key]), `${lang}.${key} placeholders`);
      }
    }
  });

  test("the strings are merged into the dashboard's table", () => {
    assert.equal(call("lookup('en', ['cost', 'title'])"), "Storage usage and cost");
    assert.equal(call("lookup('de', ['cost', 'title'])"), "Speichernutzung und Kosten");
  });
});

describe("helpers", () => {
  test("costSigned shows the sign of a change", () => {
    assert.equal(call("costSigned(0)"), "0 B");
    assert.equal(call("costSigned(2048)"), "+2.00 KiB");
    assert.equal(call("costSigned(-2048)"), "−2.00 KiB");
    assert.equal(call("costSigned('junk')"), "0 B");
  });

  test("costMoney formats the currency and survives an unknown code", () => {
    assert.match(call("costMoney(1.5, 'USD')"), /1\.50/);
    assert.match(call("costMoney(0.0345, 'USD')"), /0\.0345/);
    assert.match(call("costMoney(2, 'ZZZZ')"), /2\.00 ZZZZ/);
  });

  test("costTotalSeries sums the targets per day, oldest first", () => {
    const series = JSON.parse(call(`JSON.stringify(costTotalSeries([
      { history: [{ date: "2026-03-02", bytes: 5 }, { date: "2026-03-01", bytes: 4 }] },
      { history: [{ date: "2026-03-02", bytes: 10 }] },
      {},
    ]))`));
    assert.deepEqual(series, [{ date: "2026-03-01", bytes: 4 }, { date: "2026-03-02", bytes: 15 }]);
    assert.deepEqual(JSON.parse(call("JSON.stringify(costTotalSeries(null))")), []);
  });

  test("costChart draws finite coordinates for none, one and many points", () => {
    for (const pts of [[], [{ date: "2026-03-01", bytes: 0 }], [{ date: "2026-03-01", bytes: 10 }, { date: "2026-03-02", bytes: 20 }]]) {
      const svg = call(`costChart(${JSON.stringify(pts)})`);
      assert.ok(!/NaN|Infinity/.test(svg), svg);
      assert.equal(svg.includes('class="ov-line"'), pts.length > 0);
    }
  });
});

describe("price payload", () => {
  // Fields of the form as the user left them.
  const withFields = (fields) => {
    call(`globalThis.__fields = ${JSON.stringify(fields)}; getValue = (id) => globalThis.__fields[id] || "";`);
  };

  test("empty fields remove the price", () => {
    withFields({});
    assert.deepEqual(JSON.parse(call("JSON.stringify(costTargetPayload())")), { pricing: {} });
  });

  test("a valid entry is normalized", () => {
    withFields({ "storage-price-currency": " usd ", "storage-price-storage": "0,023", "storage-price-egress": "" });
    assert.deepEqual(JSON.parse(call("JSON.stringify(costTargetPayload())")), { pricing: { currency: "USD", storage_per_gib_month: 0.023, egress_per_gib: 0 } });
  });

  test("an invalid entry is a field error", () => {
    for (const [fields, field] of [
      [{ "storage-price-currency": "US", "storage-price-storage": "1" }, "storage-price-currency"],
      [{ "storage-price-storage": "1" }, "storage-price-currency"],
      [{ "storage-price-currency": "USD", "storage-price-storage": "-1" }, "storage-price-storage"],
      [{ "storage-price-currency": "USD", "storage-price-egress": "abc" }, "storage-price-egress"],
    ]) {
      withFields(fields);
      const err = call("(() => { try { costTargetPayload(); return null; } catch (e) { return { field: e.field, field_error: e instanceof FieldError }; } })()");
      assert.deepEqual({ field: err && err.field, isFieldError: err && err.field_error }, { field, isFieldError: true }, JSON.stringify(fields));
    }
  });
});
