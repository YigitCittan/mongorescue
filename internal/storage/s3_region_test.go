package storage

import "testing"

// TestNormalizeBucketLocation proves the AWS special cases of GetBucketLocation.
func TestNormalizeBucketLocation(t *testing.T) {
	cases := []struct {
		constraint string
		aws        bool
		want       string
	}{
		{"", true, "us-east-1"},
		{"", false, ""},
		{"EU", true, "eu-west-1"},
		{"eu-central-1", true, "eu-central-1"},
		{" US-West-2 ", false, "us-west-2"},
	}
	for _, c := range cases {
		if got := normalizeBucketLocation(c.constraint, c.aws); got != c.want {
			t.Errorf("normalizeBucketLocation(%q, %v) = %q; want %q", c.constraint, c.aws, got, c.want)
		}
	}
}
