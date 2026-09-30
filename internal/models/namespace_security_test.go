package models

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDatabaseName(t *testing.T) {
	for _, name := range []string{"shop", "Shop_2026", "shop-2", "a;b|c&d", "it's", "ümlaut", "a*b", "a<b>:c|d?", strings.Repeat("d", MaxDatabaseNameLength)} {
		if err := ValidateDatabaseName(name); err != nil {
			t.Errorf("ValidateDatabaseName(%q) = %v; want nil", name, err)
		}
	}
	for _, name := range []string{"", "a b", "a.b", "a/b", `a\b`, `a"b`, "a$b", "-", "--drop", "-h", "a\nb", "a\rb", "a\x00b", "a\tb", "a\x7fb",
		strings.Repeat("d", MaxDatabaseNameLength+1)} {
		if err := ValidateDatabaseName(name); !errors.Is(err, ErrInvalidNamespace) {
			t.Errorf("ValidateDatabaseName(%q) = %v; want ErrInvalidNamespace", name, err)
		}
	}
}

func TestValidateCollectionName(t *testing.T) {
	for _, name := range []string{"orders", "system.views", "a b", "--drop", "a.b.c", "x;y", "a*"} {
		if err := ValidateCollectionName(name); err != nil {
			t.Errorf("ValidateCollectionName(%q) = %v; want nil", name, err)
		}
	}
	for _, name := range []string{"", "$cmd", "a\nb", "a\x00b", "a\rb"} {
		if err := ValidateCollectionName(name); !errors.Is(err, ErrInvalidNamespace) {
			t.Errorf("ValidateCollectionName(%q) = %v; want ErrInvalidNamespace", name, err)
		}
	}
	if err := ValidateCollectionNames([]string{"orders", " ", ""}); err != nil {
		t.Errorf("blank entries must be ignored: %v", err)
	}
	if err := ValidateCollectionNames([]string{"orders", "a\nb"}); !errors.Is(err, ErrInvalidNamespace) {
		t.Errorf("ValidateCollectionNames = %v; want ErrInvalidNamespace", err)
	}
}
