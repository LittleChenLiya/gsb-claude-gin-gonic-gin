package main

import (
	"flag"
	"log"
	"time"
)

// Run with:
//
//	go run ./examples/bundle-upload -addr :8080 -root ./bundle-data
//
// At startup the store reclaims staging directories older than -staging-ttl
// (left behind by a crashed process). Published bundles are never touched.
func main() {
	addr := flag.String("addr", ":8080", "listen address")
	root := flag.String("root", "bundle-data", "bundle storage root directory")
	maxFiles := flag.Int("max-files", 16, "maximum files per bundle")
	maxFile := flag.Int64("max-file-bytes", 32<<20, "maximum bytes per file")
	maxTotal := flag.Int64("max-total-bytes", 128<<20, "maximum total bytes per bundle")
	ttl := flag.Duration("staging-ttl", time.Hour, "staging directory retention before startup cleanup")
	flag.Parse()

	store := NewStore(Config{
		Root:          *root,
		MaxFiles:      *maxFiles,
		MaxFileBytes:  *maxFile,
		MaxTotalBytes: *maxTotal,
		StagingTTL:    *ttl,
	}, nil)
	if n, err := store.CleanupStaging(); err != nil {
		log.Printf("staging cleanup: %v", err)
	} else if n > 0 {
		log.Printf("staging cleanup: removed %d abandoned directories", n)
	}
	log.Printf("bundle-upload example listening on %s, root=%s", *addr, *root)
	if err := NewRouter(store).Run(*addr); err != nil {
		log.Fatal(err)
	}
}
