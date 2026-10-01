package mongotools

import "testing"

// Lines as printed by mongodump and mongorestore 100.12 (timestamps, tabs and the
// two-space separators of the progress bars included).
func TestParseProgressRealLines(t *testing.T) {
	cases := []struct {
		line string
		want Progress
		ok   bool
	}{
		{"2025-06-02T09:14:01.104+0000\twriting shop.orders to archive on stdout", Progress{Kind: ProgressStarted, Namespace: "shop.orders"}, true},
		{"2025-06-02T09:14:01.104+0200\twriting shop.system.views to archive 'dump.archive'", Progress{Kind: ProgressStarted, Namespace: "shop.system.views"}, true},
		{"2025-06-02T09:14:03.105+0000\t[####....................]  shop.orders  1234/5678  (21.7%)",
			Progress{Kind: ProgressBar, Namespace: "shop.orders", Done: 1234, Total: 5678, Percent: 21.7}, true},
		{"2025-06-02T09:14:03.105+0000\t[........................]  shop.events  0/101  (0.0%)",
			Progress{Kind: ProgressBar, Namespace: "shop.events", Done: 0, Total: 101, Percent: 0}, true},
		{"2025-06-02T09:14:05.442+0000\t[########################]  shop.orders  5678/5678  (100.0%)",
			Progress{Kind: ProgressBar, Namespace: "shop.orders", Done: 5678, Total: 5678, Percent: 100}, true},
		{"2025-06-02T09:14:05.443+0000\tdone dumping shop.orders (5678 documents)", Progress{Kind: ProgressDone, Namespace: "shop.orders", Documents: 5678}, true},
		{"2025-06-02T09:14:05.443+0000\tdone dumping shop.single (1 document)", Progress{Kind: ProgressDone, Namespace: "shop.single", Documents: 1}, true},
		// mongorestore
		{"2025-06-02T09:20:00.010+0000\tpreparing collections to restore from", Progress{}, false},
		{"2025-06-02T09:20:00.012+0000\treading metadata for shop_rescue_20250602_092000.orders from archive on stdin", Progress{}, false},
		{"2025-06-02T09:20:00.020+0000\trestoring shop_rescue_20250602_092000.orders from archive on stdin",
			Progress{Kind: ProgressStarted, Namespace: "shop_rescue_20250602_092000.orders"}, true},
		{"2025-06-02T09:20:03.021+0000\t[#####...................]  shop_rescue_20250602_092000.orders  12.3MB/48.1MB  (25.6%)",
			Progress{Kind: ProgressBar, Namespace: "shop_rescue_20250602_092000.orders", Done: 12.3 * (1 << 20), Total: 48.1 * (1 << 20), Bytes: true, Percent: 25.6}, true},
		{"2025-06-02T09:20:03.021+0000\t[........................]  shop.tiny  512B/1.00KB  (50.0%)",
			Progress{Kind: ProgressBar, Namespace: "shop.tiny", Done: 512, Total: 1024, Bytes: true, Percent: 50}, true},
		{"2025-06-02T09:20:09.300+0000\tfinished restoring shop_rescue_20250602_092000.orders (100000 documents, 3 failures)",
			Progress{Kind: ProgressDone, Namespace: "shop_rescue_20250602_092000.orders", Documents: 100000, Failures: 3}, true},
		{"2025-06-02T09:20:09.300+0000\tfinished restoring shop.one (1 document, 0 failures)", Progress{Kind: ProgressDone, Namespace: "shop.one", Documents: 1}, true},
		{"2025-06-02T09:20:09.400+0000\trestoring indexes for collection shop_rescue_20250602_092000.orders from metadata", Progress{}, false},
		{"2025-06-02T09:20:09.401+0000\trestoring users from archive on stdin", Progress{}, false},
		{"2025-06-02T09:20:09.500+0000\t100000 document(s) restored successfully. 3 document(s) failed to restore.", Progress{}, false},
		{"2025-06-02T09:20:09.500+0000\tno indexes to restore for collection shop.one", Progress{}, false},
		// Without a timestamp, and garbage.
		{"[##..]  a.b  1/2  (50.0%)", Progress{Kind: ProgressBar, Namespace: "a.b", Done: 1, Total: 2, Percent: 50}, true},
		{"[##..] broken bar", Progress{}, false},
		{"", Progress{}, false},
		{"Failed: error connecting to db server: no reachable servers", Progress{}, false},
	}
	for _, c := range cases {
		got, ok := ParseProgress(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseProgress(%q) = %+v, %v; want %+v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func FuzzParseProgress(f *testing.F) {
	f.Add("2025-06-02T09:14:03.105+0000\t[####....]  shop.orders  1234/5678  (21.7%)")
	f.Add("finished restoring a.b (1 documents, 0 failures)")
	f.Fuzz(func(t *testing.T, line string) {
		p, ok := ParseProgress(line)
		if !ok {
			return
		}
		if p.Namespace == "" || p.Kind < ProgressStarted || p.Kind > ProgressDone || p.Done < 0 || p.Total < 0 {
			t.Fatalf("ParseProgress(%q) = %+v", line, p)
		}
	})
}
