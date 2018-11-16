package strategy

import (
	"context"
	"time"

	peerstore "gx/ipfs/QmZR2XWVVBCtbgBWnQhWk2xcQfaR3W8faQPriAiaaj7rsr/go-libp2p-peerstore"
	peer "gx/ipfs/QmdVrMn1LhB4ybb8hMVaMLXnA8XRSewMnK6YqXKXoTcRvN/go-libp2p-peer"

	"github.com/Conscience/protocol/log"
	"github.com/Conscience/protocol/util"
)

type peerPool struct {
	peers       chan *PeerConnection
	chProviders <-chan peerstore.PeerInfo
	needNewPeer chan struct{}
	ctx         context.Context
	cancel      func()
}

func newPeerPool(ctx context.Context, node INode, repoID string, concurrentConns int) (*peerPool, error) {
	cid, err := util.CidForString(repoID)
	if err != nil {
		return nil, err
	}

	ctxInner, cancel := context.WithCancel(ctx)

	p := &peerPool{
		peers:       make(chan *PeerConnection, concurrentConns),
		chProviders: node.FindProvidersAsync(ctxInner, cid, 999),
		needNewPeer: make(chan struct{}),
		ctx:         ctxInner,
		cancel:      cancel,
	}

	go func() {
		defer log.Errorln("closing goroutine 1")

		for {
			select {
			case <-p.needNewPeer:
			case <-p.ctx.Done():
				return
			}

			var peerConn *PeerConnection
			for {
				var peerID peer.ID
				select {
				case peerInfo, open := <-p.chProviders:
					if !open {
						log.Warnf("PROVIDERS CHANNEL IS CLOSED")
						p.chProviders = node.FindProvidersAsync(ctxInner, cid, 999)
						continue
					}
					log.Infof("FOUND PEER %+v", peerInfo)
					peerID = peerInfo.ID
				case <-p.ctx.Done():
					return
				}

				_peerConn, err := NewPeerConnection(node, peerID, repoID)
				if err != nil {
					log.Errorln("[peer pool] error opening NewPeerConnection", err)
					time.Sleep(1 * time.Second)
					continue
				}
				peerConn = _peerConn
				break
			}

			select {
			case p.peers <- peerConn:
			case <-p.ctx.Done():
				return
			}
		}
	}()

	go func() {
		defer log.Errorln("closing goroutine 2")

		for i := 0; i < concurrentConns; i++ {
			select {
			case <-p.ctx.Done():
				return
			case p.needNewPeer <- struct{}{}:
			}
		}
	}()

	return p, nil
}

func (p *peerPool) Close() error {
	log.Errorln("peerPool.Close()")
	p.cancel()

	p.needNewPeer = nil
	p.chProviders = nil
	p.peers = nil

	return nil
}

func (p *peerPool) GetConn() *PeerConnection {
	select {
	case x := <-p.peers:
		return x
	case <-p.ctx.Done():
		return nil
	}
}

func (p *peerPool) ReturnConn(conn *PeerConnection, strike bool) {
	if strike {
		select {
		case p.needNewPeer <- struct{}{}:
		case <-p.ctx.Done():
		}

	} else {
		select {
		case p.peers <- conn:
		case <-p.ctx.Done():
		}
	}
}
