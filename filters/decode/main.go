package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/src-d/go-git.v4"

	"../../config"
	"../../swarm"
)

var GIT_DIR string = os.Getenv("GIT_DIR")

func main() {
	repoID, err := getRepoID()
	if err != nil {
		panic(err)
	}

	cfg, err := config.ReadConfig()
	if err != nil {
		panic(err)
	}

	client, err := swarm.NewRPCClient(cfg.RPCClient.Network, cfg.RPCClient.Host)
	if err != nil {
		panic(err)
	}

	// First, make sure we have all of the chunks.  Any missing chunks are downloaded from the Node
	// in parallel.
	chch := make(chan chan string)
	chErr := make(chan error)
	chDone := make(chan struct{})

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			objectIDStr := scanner.Text()
			objectIDStr = strings.TrimSpace(objectIDStr)

			// break on empty string
			if len(objectIDStr) == 0 {
				break
			}

			ch := make(chan string)
			chch <- ch

			_, err = os.Stat(filepath.Join(GIT_DIR, swarm.CONSCIENCE_DATA_SUBDIR, objectIDStr))
			if err != nil {
				// file doesn't exist

				err := downloadChunk(client, repoID, objectIDStr)
				if err != nil {
					chErr <- err
					return
				}
			}
			ch <- objectIDStr
		}
		if err = scanner.Err(); err != nil {
			chErr <- err
			return
		}

		close(chch)
	}()

	// Second, loop through the objectIDs in stdin again, emitting each chunk's data serially.
	go func() {
		for ch := range chch {
			objectIDStr := <-ch

			f, err := os.Open(filepath.Join(GIT_DIR, swarm.CONSCIENCE_DATA_SUBDIR, objectIDStr))
			if err != nil {
				chErr <- err
				return
			}
			defer f.Close()

			_, err = io.Copy(os.Stdout, f)
			if err != nil {
				chErr <- err
				return
			}
		}

		close(chDone)
	}()

	select {
	case err := <-chErr:
		panic(err)
	case <-chDone:
	}
}

func getRepoID() (string, error) {
	repo, err := git.PlainOpen(filepath.Dir(GIT_DIR))
	if err != nil {
		return "", err
	}

	cfg, err := repo.Config()
	if err != nil {
		return "", err
	}

	section := cfg.Raw.Section("conscience")
	if section == nil {
		return "", fmt.Errorf("missing conscience config in .git/config")
	}

	repoID := section.Option("repoid")
	if repoID == "" {
		return "", fmt.Errorf("missing conscience config in .git/config")
	}
	return repoID, nil
}

func downloadChunk(client *swarm.RPCClient, repoID string, objectIDStr string) error {
	objectID, err := hex.DecodeString(objectIDStr)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Downloading chunk %v...\n", objectIDStr)

	objectStream, err := client.GetObject(repoID, objectID)
	if err != nil {
		return err
	}
	defer objectStream.Close()

	dataDir := filepath.Join(GIT_DIR, "data")
	err = os.MkdirAll(dataDir, 0777)
	if err != nil {
		return err
	}

	chunkPath := filepath.Join(dataDir, objectIDStr)
	f, err := os.Create(chunkPath)
	if err != nil {
		return err
	}
	defer f.Close()

	hasher := sha256.New()
	reader := io.TeeReader(objectStream, hasher)

	copied, err := io.Copy(f, reader)
	if err != nil {
		os.Remove(chunkPath)
		return err
	} else if copied != objectStream.Len() {
		os.Remove(chunkPath)
		return fmt.Errorf("copied (%v) != objectLen (%v)", copied, objectStream.Len())
	} else if !bytes.Equal(objectID, hasher.Sum(nil)) {
		os.Remove(chunkPath)
		return fmt.Errorf("checksum error (objectID: %v)", objectIDStr)
	}

	return nil
}
