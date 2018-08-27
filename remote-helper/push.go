package main

import (
	"context"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	gitplumbing "gopkg.in/src-d/go-git.v4/plumbing"
)

func push(srcRefName string, destRefName string) error {
	force := strings.HasPrefix(srcRefName, "+")
	if force {
		srcRefName = srcRefName[1:]
	}

	// Tell the node to announce the new content so that replicator nodes can find and pull it.
	// @@TODO: give context a timeout and make it configurable
	log.Infof("announcing repo content")
	ctx, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	err := client.AnnounceRepoContent(context.Background(), repoID)
	if err != nil {
		return err
	}

	srcRef, err := Repo.Reference(gitplumbing.ReferenceName(srcRefName), false)
	if err != nil {
		return errors.WithStack(err)
	}

	commitHash := srcRef.Hash().String()

	// @@TODO: give context a timeout and make it configurable
	ctx, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	log.Infof("updating ref")
	err = client.UpdateRef(ctx, repoID, destRefName, commitHash)
	if err != nil {
		return err
	}

	// @@TODO: give context a timeout and make it configurable
	ctx, cancel3 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel3()
	log.Infof("requesting replication")
	err = client.RequestReplication(ctx, repoID)
	log.Infof("done!")
	return err
}
