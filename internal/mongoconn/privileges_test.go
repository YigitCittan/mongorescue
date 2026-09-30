package mongoconn

import "testing"

func TestGrantsBypass(t *testing.T) {
	str := func(s string) *string { return &s }
	priv := func(db, coll *string, any bool, actions ...string) privilege {
		var p privilege
		p.Resource.DB, p.Resource.Collection, p.Resource.AnyResource = db, coll, any
		p.Actions = actions
		return p
	}
	for _, tc := range []struct {
		name  string
		privs []privilege
		want  bool
	}{
		{"root (anyResource)", []privilege{priv(nil, nil, true, "bypassDocumentValidation", "insert")}, true},
		{"restore role (every database)", []privilege{priv(str(""), str(""), false, "bypassDocumentValidation")}, true},
		{"dbAdmin on the target", []privilege{priv(str("shop_rescue"), str(""), false, "bypassDocumentValidation")}, true},
		{"another database", []privilege{priv(str("other"), str(""), false, "bypassDocumentValidation")}, false},
		{"one collection only", []privilege{priv(str("shop_rescue"), str("orders"), false, "bypassDocumentValidation")}, false},
		{"readWrite", []privilege{priv(str("shop_rescue"), str(""), false, "find", "insert", "update")}, false},
		{"none", nil, false},
	} {
		if got := grantsBypass(tc.privs, "shop_rescue"); got != tc.want {
			t.Errorf("%s: grantsBypass = %v; want %v", tc.name, got, tc.want)
		}
	}
}
