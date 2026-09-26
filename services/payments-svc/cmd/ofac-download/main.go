// Command ofac-download fetches the current OFAC SDN list (sdn.csv, add.csv,
// alt.csv, sdn_comments.csv) from treasury.gov into a directory, with
// moov-io/watchman's downloader.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	moovlog "github.com/moov-io/base/log"
	"github.com/moov-io/watchman/pkg/sources/ofac"
)

func main() {
	dir := flag.String("dir", "data/ofac", "where to write the files")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	files, err := ofac.Download(ctx, moovlog.NewDefaultLogger(), "")
	if err != nil {
		log.Fatal(err)
	}
	defer files.Close()
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	for name, rc := range files {
		f, err := os.Create(filepath.Join(*dir, name))
		if err != nil {
			log.Fatal(err)
		}
		n, err := io.Copy(f, rc)
		if err != nil {
			log.Fatal(err)
		}
		f.Close()
		log.Printf("%s: %d bytes", name, n)
	}
}
