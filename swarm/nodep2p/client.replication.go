package nodep2p

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/ipfs/go-cid"
	netp2p "github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-peerstore"
	"github.com/libp2p/go-libp2p-protocol"

	"github.com/Conscience/protocol/log"
	. "github.com/Conscience/protocol/swarm/wire"
	"github.com/Conscience/protocol/util"
)

type replicationNode interface {
	ID() peer.ID
	FindProvidersAsync(ctx context.Context, key cid.Cid, count int) <-chan peerstore.PeerInfo
	NewStream(ctx context.Context, peerID peer.ID, pids ...protocol.ID) (netp2p.Stream, error)
}

// Finds replicator nodes on the network that are hosting the given repository and issues requests
// to them to pull from our local copy.
func RequestReplication(ctx context.Context, n replicationNode, repoID string) (<-chan Progress, error) {
	c, err := util.CidForString("replicate:" + repoID)
	if err != nil {
		return nil, err
	}

	// @@TODO: configurable timeout
	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	peerChs := make(map[peer.ID]chan Progress)
	for provider := range n.FindProvidersAsync(ctxTimeout, c, 8) {
		if provider.ID == n.ID() {
			continue
		}

		peerCh := make(chan Progress)
		go requestPeerReplication(ctx, n, repoID, provider.ID, peerCh)
		peerChs[provider.ID] = peerCh
	}

	chProgress := make(chan Progress)
	go combinePeerChs(peerChs, chProgress)

	return chProgress, nil
}

func requestPeerReplication(ctx context.Context, n replicationNode, repoID string, peerID peer.ID, peerCh chan Progress) {
	var err error

	defer func() {
		defer close(peerCh)
		if err != nil {
			log.Errorf("[request replication error: %v]", err)
			peerCh <- Progress{Error: err}
		}
	}()

	stream, err := n.NewStream(ctx, peerID, REPLICATION_PROTO)
	if err != nil {
		return
	}
	defer stream.Close()

	err = WriteStructPacket(stream, &ReplicationRequest{RepoID: repoID})
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			peerCh <- Progress{Error: ctx.Err()}
			return
		default:
		}

		p := Progress{}
		err = ReadStructPacket(stream, &p)
		if err != nil {
			return
		}

		// Convert from an over-the-wire error (a string) to a native Go error
		if p.ErrorMsg != "" {
			p.Error = errors.New(p.ErrorMsg)
		}

		peerCh <- p

		if p.Done == true || p.Error != nil {
			return
		}
	}
}

func combinePeerChs(peerChs map[peer.ID]chan Progress, progressCh chan Progress) {
	defer close(progressCh)

	if len(peerChs) == 0 {
		err := errors.Errorf("no replicators available")
		progressCh <- Progress{Error: err}
		return
	}

	maxPercent := 0
	chPercent := make(chan int)
	someoneFinished := false
	wg := &sync.WaitGroup{}
	chDone := make(chan struct{})

	go func() {
		defer close(chDone)
		for p := range chPercent {
			if maxPercent < p {
				maxPercent = p
				progressCh <- Progress{Current: uint64(maxPercent), Total: 100}
			}
		}
	}()

	for _, peerCh := range peerChs {
		wg.Add(1)
		go func(peerCh chan Progress) {
			defer wg.Done()
			for progress := range peerCh {
				if someoneFinished {
					break
				}
				if progress.Done == true {
					someoneFinished = true
				}
				if progress.Error != nil {
					return
				}
				percent := 0
				if progress.Total > 0 {
					percent = int(100 * progress.Current / progress.Total)
				}
				chPercent <- percent
			}
		}(peerCh)
	}

	wg.Wait()
	close(chPercent)
	<-chDone

	if !someoneFinished {
		err := errors.Errorf("every replicator failed to replicate repo")
		progressCh <- Progress{Error: err}
	}
}
