package main

import (
	"context"
	"io"
	"sync"

	"github.com/pkg/errors"
	gitplumbing "gopkg.in/src-d/go-git.v4/plumbing"
)

var inflightLimiter = make(chan struct{}, 5)

func init() {
	for i := 0; i < 5; i++ {
		inflightLimiter <- struct{}{}
	}
}

func fetch(hash gitplumbing.Hash) error {
	wg := &sync.WaitGroup{}
	chErr := make(chan error)

	wg.Add(1)
	go recurseObject(hash, wg, chErr)

	chDone := make(chan struct{})
	go func() {
		defer close(chDone)
		wg.Wait()
	}()

	select {
	case <-chDone:
		return nil
	case err := <-chErr:
		return err
	}
}

func recurseObject(hash gitplumbing.Hash, wg *sync.WaitGroup, chErr chan error) {
	defer wg.Done()

	objType, err := fetchAndWriteObject(hash)
	if err != nil {
		chErr <- err
		return
	}

	// If the object is a tree or commit, make sure we have its children
	switch objType {
	case gitplumbing.TreeObject:
		tree, err := Repo.TreeObject(hash)
		if err != nil {
			chErr <- errors.WithStack(err)
			return
		}

		for _, entry := range tree.Entries {
			wg.Add(1)
			go recurseObject(entry.Hash, wg, chErr)
		}

	case gitplumbing.CommitObject:
		commit, err := Repo.CommitObject(hash)
		if err != nil {
			chErr <- errors.WithStack(err)
			return
		}

		if commit.NumParents() > 0 {
			for _, phash := range commit.ParentHashes {
				wg.Add(1)
				go recurseObject(phash, wg, chErr)
			}
		}

		wg.Add(1)
		go recurseObject(commit.TreeHash, wg, chErr)
	}
}

func fetchAndWriteObject(hash gitplumbing.Hash) (gitplumbing.ObjectType, error) {
	<-inflightLimiter
	defer func() { inflightLimiter <- struct{}{} }()

	obj, err := Repo.Object(gitplumbing.AnyObject, hash)
	// The object has already been downloaded
	if err == nil {
		return obj.Type(), nil
	}

	// Fetch an object stream from the node via RPC
	// @@TODO: give context a timeout and make it configurable
	objectStream, err := client.GetObject(context.Background(), repoID, hash[:])
	if err != nil {
		return 0, errors.WithStack(err)
	}
	defer objectStream.Close()

	// Write the object to disk
	{
		newobj := Repo.Storer.NewEncodedObject() // returns a &plumbing.MemoryObject{}
		newobj.SetType(objectStream.Type())

		w, err := newobj.Writer()
		if err != nil {
			return 0, errors.WithStack(err)
		}

		copied, err := io.Copy(w, objectStream)
		if err != nil {
			return 0, errors.WithStack(err)
		} else if uint64(copied) != objectStream.Len() {
			return 0, errors.Errorf("object stream bad length (copied: %v, object length: %v)", copied, objectStream.Len())
		}

		err = w.Close()
		if err != nil {
			return 0, errors.WithStack(err)
		}

		// Check the checksum
		if hash != newobj.Hash() {
			return 0, errors.Errorf("bad checksum for piece %v", hash.String())
		}

		// Write the object to disk
		_, err = Repo.Storer.SetEncodedObject(newobj)
		if err != nil {
			return 0, errors.WithStack(err)
		}
	}
	return objectStream.Type(), nil
}
