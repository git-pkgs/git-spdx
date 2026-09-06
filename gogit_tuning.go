package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/go-git/go-billy/v6/osfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

const (
	defaultGoGitObjectBuffer = 64
	defaultGoGitObjectBatch  = 64
)

var (
	goGitMemoryIndex  = newOption(false)
	goGitMmap         = newOption(false)
	goGitCacheBytes   = newOption(uint64(cache.DefaultMaxSize))
	goGitCacheShards  = newOption(1)
	goGitObjectInfos  = newOption(true)
	goGitObjectBuffer = newOption(defaultGoGitObjectBuffer)
	goGitObjectBatch  = newOption(defaultGoGitObjectBatch)
)

type goGitObjectTask struct {
	info filesystem.ObjectInfo
	oid  string
}

func configureGoGit(r *gogit.Repository) error {
	if !*goGitMemoryIndex && !*goGitMmap && *goGitCacheBytes == uint64(cache.DefaultMaxSize) && *goGitCacheShards <= 1 {
		return nil
	}
	fs := r.Storer.(*filesystem.Storage).Filesystem()
	if *goGitMmap {
		objects, err := fs.Chroot("objects")
		if err != nil {
			return err
		}
		fs = dotgit.NewRepositoryFilesystem(
			osfs.New(fs.Root(), osfs.WithBoundOS(), osfs.WithMmap()),
			osfs.New(filepath.Dir(objects.Root()), osfs.WithBoundOS(), osfs.WithMmap()),
		)
	}
	if err := r.Close(); err != nil {
		return err
	}
	objectCache := cache.Object(cache.NewObjectLRU(cache.FileSize(*goGitCacheBytes)))
	if *goGitCacheShards > 1 {
		objectCache = cache.NewShardedObjectLRU(cache.FileSize(*goGitCacheBytes), *goGitCacheShards)
	}
	r.Storer = filesystem.NewStorageWithOptions(fs, objectCache, filesystem.Options{
		UseInMemoryIdx: *goGitMemoryIndex,
	})
	return nil
}

func feedBlobsGoGitParallel(repo string, jobs chan<- job, skip func(string), limit func(string) int64, workers int) (int, error) {
	if !*goGitObjectInfos {
		return feedBlobsGoGitParallelObjects(repo, jobs, skip, limit, workers)
	}
	return feedBlobsGoGitParallelObjectInfos(repo, jobs, skip, limit, workers)
}

func feedBlobsGoGitParallelObjectInfos(repo string, jobs chan<- job, skip func(string), limit func(string) int64, workers int) (int, error) {
	r, err := openGoGit(repo)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	storage, ok := r.Storer.(*filesystem.Storage)
	if !ok {
		return 0, fmt.Errorf("go-git filesystem storage required")
	}
	iter, err := storage.IterObjectInfos(plumbing.BlobObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batchSize := max(1, *goGitObjectBatch)
	bufferedBatches := (*goGitObjectBuffer + batchSize - 1) / batchSize
	objects := make(chan []goGitObjectTask, bufferedBatches)
	var wg sync.WaitGroup
	var once sync.Once
	var readErr error
	for range workers {
		wg.Go(func() {
			reader := storage.NewObjectInfoReader()
			defer func() {
				if err := reader.Close(); err != nil {
					once.Do(func() { readErr = err; cancel() })
				}
			}()
			for batch := range objects {
				for _, task := range batch {
					obj, err := reader.EncodedObject(task.info)
					if err != nil {
						once.Do(func() { readErr = err; cancel() })
						return
					}
					if obj.Size() != task.info.Size {
						err = fmt.Errorf("object %s size changed during enumeration: %d != %d", task.info.Hash, obj.Size(), task.info.Size)
						once.Do(func() { readErr = err; cancel() })
						return
					}
					data, err := readGoGitBlob(obj)
					if err != nil {
						once.Do(func() { readErr = err; cancel() })
						return
					}
					select {
					case jobs <- job{oid: task.oid, data: data}:
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}
	total := 0
	batch := make([]goGitObjectTask, 0, batchSize)
enumerate:
	for {
		var info filesystem.ObjectInfo
		info, err = iter.Next()
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		total++
		oid := info.Hash.String()
		if info.Size > limit(oid) {
			skip(oid)
			continue
		}
		batch = append(batch, goGitObjectTask{info: info, oid: oid})
		if len(batch) == batchSize {
			select {
			case objects <- batch:
				batch = make([]goGitObjectTask, 0, batchSize)
			case <-ctx.Done():
				break enumerate
			}
		}
	}
	if err == nil && len(batch) != 0 {
		select {
		case objects <- batch:
		case <-ctx.Done():
		}
	}
	close(objects)
	wg.Wait()
	if readErr != nil {
		return total, readErr
	}
	return total, err
}

func feedBlobsGoGitParallelObjects(repo string, jobs chan<- job, skip func(string), limit func(string) int64, workers int) (int, error) {
	r, err := openGoGit(repo)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	iter, err := r.Storer.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objects := make(chan plumbing.EncodedObject)
	var wg sync.WaitGroup
	var once sync.Once
	var readErr error
	for range workers {
		wg.Go(func() {
			for obj := range objects {
				data, err := readGoGitBlob(obj)
				if err != nil {
					once.Do(func() { readErr = err; cancel() })
					return
				}
				select {
				case jobs <- job{oid: obj.Hash().String(), data: data}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	total := 0
enumerate:
	for {
		var obj plumbing.EncodedObject
		obj, err = iter.Next()
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		total++
		oid := obj.Hash().String()
		if obj.Size() > limit(oid) {
			skip(oid)
			continue
		}
		select {
		case objects <- obj:
		case <-ctx.Done():
			break enumerate
		}
	}
	close(objects)
	wg.Wait()
	if readErr != nil {
		return total, readErr
	}
	return total, err
}

func readGoGitBlob(obj plumbing.EncodedObject) ([]byte, error) {
	if memory, ok := obj.(interface{ Bytes() []byte }); ok {
		data := memory.Bytes()
		if int64(len(data)) != obj.Size() {
			return nil, fmt.Errorf("memory object size mismatch: %d != %d", len(data), obj.Size())
		}
		return data, nil
	}
	r, err := obj.Reader()
	if err != nil {
		return nil, err
	}
	data := make([]byte, obj.Size())
	_, err = io.ReadFull(r, data)
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	return data, closeErr
}
