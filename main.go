package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/boltdb/bolt"
	"github.com/docker/go-plugins-helpers/volume/shim"
	"github.com/pkg/errors"
)

const root = "/var/lib/butter"

func main() {
	flDevice := flag.String("device", "", "device to use -- must be pre-configured with btrfs")
	flMountOpts := flag.String("mount-opts", "", "mount options to mount the btrfs device")
	flag.Parse()

	device := *flDevice
	if device == "" {
		device = os.Getenv("BTRFS_DEVICE")
		if device == "" {
			exitOnError(errors.New("invalid setting"), "must specify a device to use")
		}
	}

	mountOpts := *flMountOpts
	if mountOpts == "" {
		mountOpts = os.Getenv("BTRFS_MOUNT_OPTIONS")
	}

	err := os.MkdirAll(filepath.Join(root, "data"), 0755)
	exitOnError(err, "error creating root dir")

	err = unix.Mount(device, filepath.Join(root, "data"), "btrfs", 0, mountOpts)
	exitOnError(err, "error mounting btrfs device")
	defer unix.Unmount(filepath.Join(root, "data"), 0)

	db, err := bolt.Open(filepath.Join(root, "volumes.db"), 600, &bolt.Options{
		Timeout: 30 * time.Second,
	})
	exitOnError(err, "error opening volumes database")
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(volumesBucket)
		return err
	})
	exitOnError(err, "error creating boltdb bucket")

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
