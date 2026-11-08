// Unit tests of the dashboard's pure helpers (formatting, pagination, cron text,
// escaping). Run with: node --test web/test/
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { evalIn, loadScripts } from "./harness.mjs";

let ctx;
const call = (expr) => evalIn(ctx, expr);

before(() => {
  ctx = loadScripts(["i18n.js", "app.js"]);
});

describe("formatBytes", () => {
  test("uses binary units with three significant digits", () => {
    assert.equal(call("formatBytes(0)"), "0 B");
    assert.equal(call("formatBytes(1023)"), "1,023 B");
    assert.equal(call("formatBytes(1024)"), "1.00 KiB");
    assert.equal(call("formatBytes(1536)"), "1.50 KiB");
    assert.equal(call("formatBytes(10 * 1024 * 1024)"), "10.0 MiB");
    assert.equal(call("formatBytes(500 * 1024 * 1024)"), "500 MiB");
    assert.equal(call("formatBytes(5 * 1024 ** 3)"), "5.00 GiB");
  });
  test("never shows 1024 of a unit", () => {
    assert.equal(call("formatBytes(1024 * 1024 - 1)"), "1.00 MiB");
  });
  test("treats junk as zero", () => {
    for (const v of ["null", "undefined", "NaN", "-5", "'abc'"]) assert.equal(call(`formatBytes(${v})`), "0 B", v);
  });
});

describe("formatSpan and formatDuration", () => {
  test("formatSpan is compact", () => {
    assert.equal(call("formatSpan(0)"), "1m");
    assert.equal(call("formatSpan(90 * 60000)"), "1h 30m");
    assert.equal(call("formatSpan(2 * 3600000)"), "2h");
    assert.equal(call("formatSpan(26 * 3600000)"), "1d 2h");
    assert.equal(call("formatSpan(48 * 3600000)"), "2d");
  });
  test("formatDuration picks the unit", () => {
    assert.equal(call("formatDuration(0.2)"), "200 ms");
    assert.equal(call("formatDuration(5)"), "5.0 s");
    assert.equal(call("formatDuration(125)"), "2m 5s");
    assert.equal(call("formatDuration(7384)"), "2h 3m");
  });
});

describe("dates", () => {
  test("parseDate refuses empty and zero dates", () => {
    assert.equal(call("parseDate('')"), null);
    assert.equal(call("parseDate('0001-01-01T00:00:00Z')"), null);
    assert.equal(call("parseDate('garbage')"), null);
    assert.equal(call("parseDate('2026-01-02T03:04:05Z').getTime()"), Date.UTC(2026, 0, 2, 3, 4, 5));
  });
  test("formatRelative reads naturally", () => {
    assert.equal(call("formatRelative(new Date(Date.now() - 2 * 3600 * 1000))"), "2 hours ago");
    assert.equal(call("formatRelative(new Date(Date.now() + 3 * 86400 * 1000 + 5000))"), "in 3 days");
  });
});

describe("version and counts", () => {
  test("formatVersion prefixes releases only", () => {
    assert.equal(call("formatVersion('0.4.1')"), "v0.4.1");
    assert.equal(call("formatVersion('dev')"), "dev");
    assert.equal(call("formatVersion('  ')"), "");
    assert.equal(call("formatVersion(null)"), "");
  });
  test("formatCount groups digits", () => {
    assert.equal(call("formatCount(1234567)"), "1,234,567");
  });
});

describe("pageItems", () => {
  const items = (page, last) => JSON.parse(call(`JSON.stringify(pageItems(${page}, ${last}))`));
  test("shows every page up to seven", () => {
    assert.deepEqual(items(1, 1), [1]);
    assert.deepEqual(items(3, 7), [1, 2, 3, 4, 5, 6, 7]);
  });
  test("collapses the far end near the start", () => {
    assert.deepEqual(items(1, 10), [1, 2, 3, 4, "gap", 10]);
    assert.deepEqual(items(3, 10), [1, 2, 3, 4, "gap", 10]);
  });
  test("collapses both ends in the middle", () => {
    assert.deepEqual(items(5, 10), [1, "gap", 4, 5, 6, "gap", 10]);
  });
  test("collapses the near end near the finish", () => {
    assert.deepEqual(items(8, 10), [1, "gap", 7, 8, 9, 10]);
    assert.deepEqual(items(10, 10), [1, "gap", 7, 8, 9, 10]);
  });
  test("always starts at 1, ends at last and is strictly increasing", () => {
    for (let last = 1; last <= 40; last++) {
      for (let page = 1; page <= last; page++) {
        const r = items(page, last).filter((x) => x !== "gap");
        assert.equal(r[0], 1);
        assert.equal(r.at(-1), last);
        assert.ok(r.includes(page), `${page}/${last}`);
        assert.deepEqual(r, [...new Set(r)].sort((a, b) => a - b));
      }
    }
  });
});

describe("cron and durations", () => {
  test("describeCron", () => {
    assert.equal(call("describeCron('')"), "");
    assert.equal(call("describeCron('@daily')"), call("t('cron.daily')"));
    assert.equal(call("describeCron('0 3 * * *')"), "Every day at 03:00");
    assert.equal(call("describeCron('*/15 * * * *')"), call("tf('cron.every_n_minutes', { n: 15 })"));
    assert.equal(call("describeCron('not a cron')"), "");
    assert.equal(call("describeCron('0 3 1 1 *')"), "");
  });
  test("parseGoDuration and humanDuration", () => {
    assert.equal(call("parseGoDuration('90s')"), 90000);
    assert.equal(call("parseGoDuration('1h30m')"), 5400000);
    assert.ok(Number.isNaN(call("parseGoDuration('soon')")));
    assert.equal(typeof call("humanDuration(5400000)"), "string");
  });
});

describe("escapeHtml", () => {
  test("escapes the five characters and tolerates null", () => {
    assert.equal(call(`escapeHtml('<a href="x" onclick=\\'y\\'>&')`), "&lt;a href=&quot;x&quot; onclick=&#39;y&#39;&gt;&amp;");
    assert.equal(call("escapeHtml(null)"), "");
    assert.equal(call("escapeHtml(undefined)"), "");
    assert.equal(call("escapeHtml(42)"), "42");
  });
});

describe("can", () => {
  test("compares the effective scope", () => {
    call("auth.scope = 'operator'");
    assert.equal(call("can('read')"), true);
    assert.equal(call("can('operator')"), true);
    assert.equal(call("can('admin')"), false);
    call("auth.scope = ''");
  });
});
