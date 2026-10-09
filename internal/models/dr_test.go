package models_test

import (
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func s3Target(endpoint, region, key string) *models.StorageTarget {
	return &models.StorageTarget{Type: models.StorageS3, S3: &models.S3Target{Endpoint: endpoint, Region: region, Bucket: "b", AccessKeyID: key}}
}

// TestDRRegion proves that the region label wins over the S3 region, that "auto"
// is unknown, and that regions compare case-insensitively.
func TestDRRegion(t *testing.T) {
	cases := []struct {
		target *models.StorageTarget
		want   string
	}{
		{s3Target("", "eu-central-1", "A"), "eu-central-1"},
		{s3Target("", "auto", "A"), ""},
		{&models.StorageTarget{Type: models.StorageS3, Region: " US-East-1 ", S3: &models.S3Target{Region: "auto"}}, "us-east-1"},
		{&models.StorageTarget{Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/b"}}, ""},
		{&models.StorageTarget{Type: models.StorageLocal, Region: "dc-ams"}, "dc-ams"},
		{nil, ""},
	}
	for i, c := range cases {
		if got := c.target.DRRegion(); got != c.want {
			t.Errorf("case %d: DRRegion() = %q; want %q", i, got, c.want)
		}
	}
	if models.CrossRegion(s3Target("", "eu-west-1", "A"), s3Target("", "EU-WEST-1", "B")) {
		t.Error("the same region in another case counts as cross-region")
	}
	if models.CrossRegion(s3Target("", "eu-west-1", "A"), s3Target("", "auto", "B")) {
		t.Error("an unknown region counts as cross-region")
	}
	if !models.CrossRegion(s3Target("", "eu-west-1", "A"), s3Target("", "us-east-2", "B")) {
		t.Error("two regions do not count as cross-region")
	}
}

// TestSameCredentials proves which pairs of targets share a failure domain.
func TestSameCredentials(t *testing.T) {
	local := &models.StorageTarget{Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/a"}}
	r2 := "https://0123abcd.r2.cloudflarestorage.com"
	cases := []struct {
		name string
		a, b *models.StorageTarget
		want bool
	}{
		{"same access key", s3Target("", "eu-west-1", "AKIA1"), s3Target("https://minio.example.com", "us-east-1", "AKIA1"), true},
		{"different keys on AWS", s3Target("", "eu-west-1", "AKIA1"), s3Target("", "us-east-1", "AKIA2"), false},
		{"default credentials on one endpoint", s3Target("", "eu-west-1", ""), s3Target("", "us-east-1", ""), true},
		{"default credentials on two endpoints", s3Target("", "eu-west-1", ""), s3Target("https://minio.example.com", "x", ""), false},
		{"one R2 account", s3Target(r2, "auto", "K1"), s3Target(r2+"/", "auto", "K2"), true},
		{"two R2 accounts", s3Target(r2, "auto", "K1"), s3Target("https://ffff.r2.cloudflarestorage.com", "auto", "K2"), false},
		{"two local targets", local, &models.StorageTarget{Type: models.StorageLocal}, true},
		{"local and S3", local, s3Target("", "eu-west-1", ""), false},
	}
	for _, c := range cases {
		if got := models.SameCredentials(c.a, c.b); got != c.want {
			t.Errorf("%s: SameCredentials = %v; want %v", c.name, got, c.want)
		}
	}
}
