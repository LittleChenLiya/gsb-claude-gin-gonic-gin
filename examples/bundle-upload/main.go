// Command bundle-upload runs the multi-file bundle upload example.
package main

import (
	"flag"
	"log"
	"time"

	"github.com/gin-gonic/gin/examples/bundle-upload/bundle"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	root := flag.String("root", "./bundle-data", "bundle storage root directory")
	maxFiles := flag.Int("max-files", 20, "maximum files per bundle")
	maxFileMB := flag.Int64("max-file-mb", 32, "maximum size of a single file in MiB")
	maxTotalMB := flag.Int64("max-total-mb", 64, "maximum total size of a bundle in MiB")
	stagingMaxAge := flag.Duration("staging-max-age", time.Hour, "remove staging dirs older than this at startup")
	flag.Parse()

	store, err := bundle.NewStore(*root, bundle.Config{
		MaxFiles:      *maxFiles,
		MaxFileBytes:  *maxFileMB << 20,
		MaxTotalBytes: *maxTotalMB << 20,
		StagingMaxAge: *stagingMaxAge,
	})
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	// Startup recovery: drop stale staging directories left behind by an
	// interrupted previous run. Published bundles are never touched.
	removed, err := store.CleanupStaging()
	if err != nil {
		log.Printf("staging cleanup: %v", err)
	} else if removed > 0 {
		log.Printf("staging cleanup: removed %d stale bundle(s)", removed)
	}

	r := bundle.NewRouter(store)
	log.Printf("listening on %s, storing bundles under %s", *addr, store.Root())
	if err := r.Run(*addr); err != nil {
		log.Fatal(err)
	}
}
