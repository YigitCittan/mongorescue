package operations

import (
	"fmt"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestCollectionCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newCollectionCache(2)
	list := func(name string) []models.BackupCollection { return []models.BackupCollection{{Name: name}} }
	c.put("a", list("a"))
	c.put("b", list("b"))
	if _, ok := c.get("a"); !ok { // a is now the most recent
		t.Fatal("a missing")
	}
	c.put("c", list("c"))
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if got, ok := c.get("a"); !ok || got[0].Name != "a" {
		t.Fatalf("a = %+v, %v", got, ok)
	}
	c.put("a", list("a2"))
	if got, _ := c.get("a"); got[0].Name != "a2" || c.len() != 2 {
		t.Fatalf("updated a = %+v (len %d)", got, c.len())
	}
}

func TestCollectionCacheConcurrentUse(t *testing.T) {
	c := newCollectionCache(8)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for j := range 100 {
				id := fmt.Sprintf("bkp_%d", (i+j)%20)
				c.put(id, []models.BackupCollection{{Name: id}})
				if got, ok := c.get(id); ok && got[0].Name != id {
					t.Errorf("%s = %+v", id, got)
				}
			}
		})
	}
	wg.Wait()
	if c.len() > 8 {
		t.Fatalf("cache holds %d entries; want at most 8", c.len())
	}
}
