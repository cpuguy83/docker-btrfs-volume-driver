package main

/*
#include <stdlib.h>
#include <dirent.h>
#include <btrfs/ioctl.h>
#include <btrfs/ctree.h>
*/
import "C"

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/boltdb/bolt"
	"github.com/containerd/btrfs"
	"github.com/docker/docker/volume"
	units "github.com/docker/go-units"
	"github.com/pkg/errors"
)
import (
	"unsafe"

	"golang.org/x/sys/unix"
)

var volumesBucket = []byte("volumes")

type driver struct {
	root string
	db   *bolt.DB
}

var notFound = errors.New("volume not found")
var volumeExists = errors.New("volume exists")

func (driver) Name() string {
	return "btrfsvolume"
}

type vol struct {
	name      string
	snapshot  string
	children  []string
	path      string
	createdAt time.Time
}

func (v vol) Name() string {
	return v.name
}

func (v vol) DriverName() string {
	return "btrfsvolume"
}

func (v vol) Path() string {
	return v.path
}

func (v vol) Mount(id string) (string, error) {
	return v.path, nil
}

func (v vol) Unmount(id string) error {
	return nil
}

func (v vol) CreatedAt() (time.Time, error) {
	return v.createdAt, nil
}

func (v vol) Status() map[string]interface{} {
	info, _ := btrfs.SubvolInfo(v.Path())
	return map[string]interface{}{
		"Parent":     v.snapshot,
		"SubvolInfo": info,
	}
}

type volJSON struct {
	Name      string
	Snapshot  string
	Children  []string
	Path      string
	CreatedAt time.Time
}

func (d *driver) Create(name string, opts map[string]string) (volume.Volume, error) {
	var v vol
	err := d.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(volumesBucket)

		if v := bucket.Get([]byte(name)); v != nil {
			return volumeExists
		}

		dir := d.volumePath(name)
		if f := opts["from"]; f != "" {
			from, err := getVolume(tx, f)
			if err != nil {
				return errors.Wrap(err, "error looking up from volume")
			}

			if err := btrfs.SubvolSnapshot(dir, d.volumePath(from.Name()), false); err != nil {
				return errors.Wrap(err, "error creating snapshot")
			}
			from.children = append(from.children, name)
			if err := saveVolume(tx, from); err != nil {
				return errors.Wrap(err, "error updating parent volume")
			}
		} else {
			if err := btrfs.SubvolCreate(dir); err != nil {
				return errors.Wrap(err, "error creating btrfs subvolume")
			}
		}

		if q := opts["quota"]; q != "" {
			size, err := units.RAMInBytes(q)
			if err != nil {
				return errors.Wrap(err, "error parsing quota size")
			}
			if size <= 0 {
				return errors.Wrap(err, "invalid quota size, must be greater than 0")
			}
			if err := setQuota(dir, size); err != nil {
				return err
			}
		}

		v = vol{
			name:     name,
			snapshot: opts["from"],
			path:     d.volumePath(name),
		}
		return saveVolume(tx, v)
	})
	return v, err
}

func (d *driver) Remove(v volume.Volume) error {
	err := d.db.Update(func(tx *bolt.Tx) error {
		vo, err := getVolume(tx, v.Name())
		if err != nil {
			if err != notFound {
				return err
			}
			return nil
		}

		if err := btrfs.SubvolDelete(d.volumePath(v.Name())); err != nil {
			if e := btrfs.IsSubvolume(d.volumePath(v.Name())); e == nil {
				return errors.Wrap(err, "error removing subvolume")
			}
		}

		if vo.snapshot != "" {
			pv, _ := getVolume(tx, vo.snapshot)
			for i, c := range pv.children {
				if c == vo.snapshot {
					pv.children = append(pv.children[:i-1], pv.children[i+1:]...)
				}
			}
			if err := saveVolume(tx, pv); err != nil {
				return errors.Wrap(err, "error updating parent container")
			}
		}
		return errors.Wrap(tx.Bucket(volumesBucket).Delete([]byte(v.Name())), "error deleting volume from db")
	})
	return err
}

func (d *driver) List() ([]volume.Volume, error) {
	var ls []volume.Volume
	err := d.db.View(func(tx *bolt.Tx) error {
		var err error
		ls, err = listVolumes(tx)
		return err
	})
	return ls, err
}

func listVolumes(tx *bolt.Tx) ([]volume.Volume, error) {
	var ls []volume.Volume
	err := tx.Bucket(volumesBucket).ForEach(func(key, value []byte) error {
		var v volJSON
		if err := json.Unmarshal(value, &v); err != nil {
			return errors.Wrap(err, "error unmarshaling volume details")
		}
		ls = append(ls, convertJSON(v))
		return nil
	})
	return ls, err
}

func convertJSON(vj volJSON) vol {
	return vol{
		name:      vj.Name,
		path:      vj.Path,
		children:  vj.Children,
		createdAt: vj.CreatedAt,
	}
}

func (d *driver) Get(name string) (volume.Volume, error) {
	var v vol
	err := d.db.View(func(tx *bolt.Tx) error {
		var err error
		v, err = getVolume(tx, name)
		if err != nil {
			return err
		}
		return nil
	})
	return v, err
}

func (d *driver) Scope() string {
	return "local"
}

func (d *driver) volumePath(name string) string {
	return filepath.Join(d.root, "data", name)
}

func getVolume(tx *bolt.Tx, name string) (vol, error) {
	b := tx.Bucket(volumesBucket).Get([]byte(name))
	if b == nil {
		return vol{}, notFound
	}

	var vj volJSON
	if err := json.Unmarshal(b, &vj); err != nil {
		return vol{}, errors.Wrap(err, "error unmarshalling volume")
	}

	return convertJSON(vj), nil
}

func saveVolume(tx *bolt.Tx, v vol) error {
	vj := volJSON{
		Name:      v.name,
		Snapshot:  v.snapshot,
		Path:      v.path,
		CreatedAt: v.createdAt,
		Children:  v.children,
	}
	b, err := json.Marshal(vj)
	if err != nil {
		return errors.Wrap(err, "error marshaling volume details")
	}
	if err := tx.Bucket(volumesBucket).Put([]byte(v.name), b); err != nil {
		return errors.Wrap(err, "error saving volume details to db")
	}
	return nil
}

func setQuota(dir string, size int64) error {
	Cpath := C.CString(dir)
	defer C.free(unsafe.Pointer(Cpath))

	dirFd := C.opendir(Cpath)
	if dirFd == nil {
		return errors.New("dir not found while setting quota")
	}
	defer C.closedir(dirFd)

	var args C.struct_btrfs_ioctl_qgroup_limit_args
	args.lim.max_referenced = C.__u64(uint64(size))
	args.lim.flags = C.BTRFS_QGROUP_LIMIT_MAX_RFER
	_, _, err := unix.Syscall(unix.SYS_IOCTL, uintptr(C.dirfd(dirFd)), C.BTRFS_IOC_QGROUP_LIMIT,
		uintptr(unsafe.Pointer(&args)))

	if err != 0 {
		return errors.Wrap(err, "error setting btrfs quota")
	}
	return nil
}
