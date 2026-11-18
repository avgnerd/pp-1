package vfs

import (
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
)

// Invalidating a storage root must drop the cached listing, folder size and temporary link of every
// path below it. Building the prefix as key + "/" doubles the root slash and matches nothing.
func TestInvalidatePathAtRootDropsDescendantCaches(t *testing.T) {
	c := cache.New(1000, 1<<20, time.Minute)
	t.Cleanup(c.Stop)
	s := &Service{Cache: c}

	keys := []string{
		listKey(1, "/sub"),
		listKey(1, "/sub/deep"),
		FolderSizeKey(1, "/sub"),
		FolderSizeKey(1, "/sub/deep"),
		TempLinkKey(1, "/sub/a.mp4"),
		TempLinkKey(1, "/sub/deep/b.mp4"),
	}
	for _, k := range keys {
		c.Set(k, 1, 1, time.Minute)
	}
	other := listKey(2, "/sub")
	c.Set(other, 1, 1, time.Minute)

	s.InvalidatePath(1, "/")

	for _, k := range keys {
		if _, ok := c.Get(k); ok {
			t.Fatalf("cache key %q survived InvalidatePath(1, \"/\")", k)
		}
	}
	if _, ok := c.Get(other); !ok {
		t.Fatal("invalidating storage 1 dropped storage 2's cache")
	}
}
