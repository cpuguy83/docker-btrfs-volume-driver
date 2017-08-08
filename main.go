package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/boltdb/bolt"
	"github.com/docker/go-plugins-helpers/volume/shim"
)

const root = "/var/lib/butter"

func main() {
	db, err := bolt.Open(filepath.Join(root, "volumes.db"), 600, &bolt.Options{
		Timeout: 30 * time.Second,
	})
	exitOnError(err, "error opening volumes database")

	h := shim.NewHandlerFromVolumeDriver(&driver{
		root: root,
		db:   db,
	})
	err = h.ServeUnix("btrfsvolume", 0)
	exitOnError(err, "error making unix socket")
}

func exitOnError(err error, msg string) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, msg+":", err)
	os.Exit(1)
}
