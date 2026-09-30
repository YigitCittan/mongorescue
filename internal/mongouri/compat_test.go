package mongouri

import "testing"

// legacyOnlyURIs were accepted by v0.7.0 and are rejected by the stricter Validate.
var legacyOnlyURIs = []string{
	"mongodb://u:pa\u00a0ss@h/db", // unencoded no-break space in the password
	"mongodb://u:pa\u3000ss@h/db", // ideographic space
	"mongodb://u:p\u2028x@h/db",   // line separator
	"mongodb://u:p\u0085x@h/db",   // NEL
	"mongodb://u:p\xffx@h/db",     // invalid UTF-8
	"mongodb://u:100%@h/db",       // raw '%'
	"mongodb://u:p%zz@h/db",       // malformed escape
	"mongodb://h{x}/db",           // host character outside RFC 3986
	"mongodb://h/?=x",             // empty option name
}

func TestValidateStoredAcceptsWhatV07Accepted(t *testing.T) {
	for _, uri := range legacyOnlyURIs {
		if err := v07Validate(uri); err != nil {
			t.Fatalf("test input %q was not valid in v0.7.0: %v", uri, err)
		}
		if err := Validate(uri); err == nil {
			t.Errorf("Validate(%q) accepted a URI only the old rules allow", uri)
		}
		if err := ValidateStored(uri); err != nil {
			t.Errorf("ValidateStored(%q) = %v; stored URIs must keep working", uri, err)
		}
	}
}

// FuzzValidateStored checks that ValidateStored accepts exactly what v0.7.0 accepted
// and that everything Validate accepts is accepted by ValidateStored too.
func FuzzValidateStored(f *testing.F) {
	for _, s := range append(append([]string{}, fuzzSeeds...), legacyOnlyURIs...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		old, stored := v07Validate(uri) == nil, ValidateStored(uri) == nil
		if old != stored {
			t.Fatalf("v0.7.0 accepted %q: %v; ValidateStored: %v", uri, old, stored)
		}
		if Validate(uri) == nil && !stored {
			t.Fatalf("Validate accepts %q but ValidateStored does not", uri)
		}
	})
}
