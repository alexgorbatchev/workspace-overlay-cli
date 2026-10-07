package main

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/hanwen/go-fuse/v2/fs"
)

type layer struct {
	root  *os.Root
	rules *pathRules
}

type view struct {
	exclude    *gitExclude
	layers     []layer
	mu         sync.Mutex
	idMu       sync.Mutex
	identities map[identity]uint64
	nextID     uint64
	created    map[identity]bool
}

type identity struct {
	layer         int
	device, inode uint64
}

func fileIdentity(index int, info os.FileInfo) identity {
	stat := info.Sys().(*syscall.Stat_t)
	return identity{layer: index, device: uint64(stat.Dev), inode: stat.Ino}
}

func (v *view) stable(name string) (fs.StableAttr, error) {
	parts, err := v.resolve(name)
	if err != nil {
		return fs.StableAttr{}, err
	}
	last := parts[len(parts)-1]
	stat := last.info.Sys().(*syscall.Stat_t)
	key := fileIdentity(last.index, last.info)
	v.idMu.Lock()
	defer v.idMu.Unlock()
	if v.identities == nil {
		v.identities = make(map[identity]uint64)
		v.nextID = 2
	}
	id, ok := v.identities[key]
	if !ok {
		id = v.nextID
		v.nextID++
		v.identities[key] = id
	}
	return fs.StableAttr{Mode: stat.Mode, Ino: id}, nil
}

type contribution struct {
	index int
	info  os.FileInfo
}

func (v *view) close() {
	for _, layer := range v.layers {
		if err := layer.root.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "ERR: close layer:", err)
		}
	}
}

func (v *view) resolve(name string) ([]contribution, error) {
	var result []contribution
	for i, layer := range v.layers {
		if i > 0 && (gitMetadata(name) || !layer.includes(name)) {
			continue
		}
		info, err := layer.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(result) > 0 {
			first := result[0].info
			if first.Mode().Type() != info.Mode().Type() || (!info.IsDir() && !info.Mode().IsRegular()) {
				return nil, fmt.Errorf("incompatible types at %s: %w", name, syscall.EIO)
			}
		}
		result = append(result, contribution{index: i, info: info})
	}
	if len(result) == 0 {
		return nil, os.ErrNotExist
	}
	return result, nil
}

func (v *view) contents(name string, parts []contribution) ([]byte, error) {
	var result []byte
	for _, part := range parts {
		data, err := v.layers[part.index].root.ReadFile(name)
		if err != nil {
			return nil, err
		}
		result = append(result, data...)
	}
	return result, nil
}

func (v *view) entries(name string) ([]os.DirEntry, error) {
	parts, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	if !parts[0].info.IsDir() {
		return nil, syscall.ENOTDIR
	}
	names := make(map[string]os.DirEntry)
	for _, part := range parts {
		entries, err := iofs.ReadDir(v.layers[part.index].root.FS(), name)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			child := path.Join(name, entry.Name())
			if part.index > 0 && (gitMetadata(child) || !v.layers[part.index].includes(child)) {
				continue
			}
			names[entry.Name()] = entry
		}
	}
	result := make([]os.DirEntry, 0, len(names))
	for _, entry := range names {
		result = append(result, entry)
	}
	return result, nil
}

func (v *view) destination(name string) (int, error) {
	parts, err := v.resolve(name)
	if err == nil {
		return parts[len(parts)-1].index, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	parents, err := v.resolve(path.Dir(name))
	if err != nil {
		return 0, err
	}
	if !parents[0].info.IsDir() {
		return 0, syscall.ENOTDIR
	}
	if parents[0].index == 0 {
		return 0, nil
	}
	index := parents[len(parents)-1].index
	if rules := v.layers[index].rules; rules != nil {
		matches, err := doublestar.Match(rules.glob, name)
		if err != nil {
			return 0, err
		}
		if !matches {
			return 0, syscall.EPERM
		}
	}
	return index, nil
}
