package swarm

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"

	cid "github.com/ipfs/go-cid"
	dstore "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p-crypto"
	host "github.com/libp2p/go-libp2p-host"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	kbucket "github.com/libp2p/go-libp2p-kbucket"
	metrics "github.com/libp2p/go-libp2p-metrics"
	netp2p "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	protocol "github.com/libp2p/go-libp2p-protocol"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/libgit2/git2go"

	"github.com/Conscience/protocol/config"
	"github.com/Conscience/protocol/log"
	"github.com/Conscience/protocol/repo"
	"github.com/Conscience/protocol/swarm/nodeeth"
	"github.com/Conscience/protocol/swarm/nodep2p"
	"github.com/Conscience/protocol/swarm/nodep2p/gittransport"
	. "github.com/Conscience/protocol/swarm/wire"
	"github.com/Conscience/protocol/util"
)

type Node struct {
	host        host.Host
	dht         *dht.IpfsDHT
	eth         *nodeeth.Client
	repoManager *RepoManager
	watchers    []*Watcher
	Config      config.Config
	Shutdown    chan struct{}

	bandwidthCounter *metrics.BandwidthCounter
}

func NewNode(ctx context.Context, cfg *config.Config) (*Node, error) {
	if cfg == nil {
		cfg = &config.DefaultConfig
	}

	privkey, err := obtainKey(cfg)
	if err != nil {
		return nil, err
	}

	err = os.MkdirAll(cfg.Node.ReplicationRoot, os.ModePerm)
	if err != nil {
		return nil, err
	}

	bandwidthCounter := metrics.NewBandwidthCounter()

	// Initialize the p2p host
	h, err := libp2p.New(ctx,
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/%v/tcp/%v", cfg.Node.P2PListenAddr, cfg.Node.P2PListenPort),
		),
		libp2p.Identity(privkey),
		libp2p.BandwidthReporter(bandwidthCounter),
		libp2p.NATPortMap(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "could not initialize libp2p host")
	}

	// Initialize the DHT
	d := dht.NewDHT(ctx, h, dsync.MutexWrap(dstore.NewMapDatastore()))
	d.Validator = blankValidator{} // Set a pass-through validator

	// Initialize the Ethereum client
	eth, err := nodeeth.NewClient(ctx, cfg)
	if err != nil {
		return nil, errors.Wrap(err, "could not initialize Ethereum client")
	}

	username, err := eth.GetUsername(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "could not fetch username from Ethereum smart contract")
	}
	log.SetField("username", username)

	n := &Node{
		host:             h,
		dht:              d,
		eth:              eth,
		repoManager:      NewRepoManager(cfg),
		watchers:         make([]*Watcher, 0),
		Config:           *cfg,
		Shutdown:         make(chan struct{}),
		bandwidthCounter: bandwidthCounter,
	}

	err = gittransport.Register(n)
	if err != nil {
		return nil, errors.Wrap(err, "could not register conscience:// git transport")
	}

	go n.periodicallyAnnounceContent(ctx) // Start a goroutine for announcing which repos and objects this Node can provide
	go n.periodicallyRequestContent(ctx)  // Start a goroutine for pulling content from repos we are replicating

	ns := nodep2p.NewServer(n)
	n.host.SetStreamHandler(nodep2p.MANIFEST_PROTO, ns.HandleManifestRequest)
	n.host.SetStreamHandler(nodep2p.PACKFILE_PROTO, ns.HandlePackfileStreamRequest)
	n.host.SetStreamHandler(nodep2p.CHUNK_PROTO, ns.HandleChunkStreamRequest)
	n.host.SetStreamHandler(nodep2p.REPLICATION_PROTO, ns.HandleReplicationRequest)
	n.host.SetStreamHandler(nodep2p.BECOME_REPLICATOR_PROTO, ns.HandleBecomeReplicatorRequest)

	// Connect to our list of bootstrap peers
	go func() {
		for _, peeraddr := range cfg.Node.BootstrapPeers {
			err = n.AddPeer(ctx, peeraddr)
			if err != nil {
				log.Errorf("[node] could not reach boostrap peer %v: %v", peeraddr, err)
			}
		}
	}()

	return n, nil
}

type blankValidator struct{}

func (blankValidator) Validate(_ string, _ []byte) error        { return nil }
func (blankValidator) Select(_ string, _ [][]byte) (int, error) { return 0, nil }

func obtainKey(cfg *config.Config) (crypto.PrivKey, error) {
	f, err := os.Open(cfg.Node.PrivateKeyFile)
	if err != nil && !os.IsNotExist(err) {
		return nil, err

	} else if err == nil {
		defer f.Close()

		data, err := ioutil.ReadFile(cfg.Node.PrivateKeyFile)
		if err != nil {
			return nil, err
		}
		return crypto.UnmarshalPrivateKey(data)
	}

	privkey, _, err := crypto.GenerateKeyPair(crypto.Secp256k1, 0)
	if err != nil {
		return nil, err
	}

	bs, err := privkey.Bytes()
	if err != nil {
		return nil, err
	}

	err = ioutil.WriteFile(cfg.Node.PrivateKeyFile, bs, 0400)
	if err != nil {
		return nil, err
	}

	return privkey, nil
}

func (n *Node) Close() error {
	close(n.Shutdown)

	err := n.host.Close()
	if err != nil {
		return errors.Wrap(err, "could not .Close libp2p host")
	}

	err = n.dht.Close()
	if err != nil {
		return errors.Wrap(err, "could not .Close libp2p DHT")
	}

	err = n.eth.Close()
	if err != nil {
		return errors.Wrap(err, "could not .Close Ethereum client")
	}

	return nil
}

func (n *Node) periodicallyRequestContent(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Debugf("[content request] starting content request")

		for _, repoID := range n.Config.Node.ReplicateRepos {
			log.Debugf("[content request] requesting repo '%v'", repoID)

			r, err := n.repoManager.EnsureLocalCheckoutExists(repoID)
			if err != nil {
				log.Warnf("[content request] error ensuring repo exists (%v): %v", repoID, err)
				continue
			}

			updatedRemotes, err := nodep2p.FetchConscienceRemote(context.TODO(), &nodep2p.FetchOptions{Repo: r})
			if err != nil {
				log.Warnf("[content request] error fetching conscience://%v remote: %v", repoID, err)
				continue
			}

			if len(updatedRemotes) > 0 {
				log.Debugf("[content request] fetched %v with updated remotes %v", strings.Join(updatedRemotes, " "))
			}
		}

		time.Sleep(time.Duration(n.Config.Node.ContentRequestInterval))
	}
}

// Periodically announces our repos and objects to the network.
func (n *Node) periodicallyAnnounceContent(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Debugf("[content announce] starting content announce")

		// Announce what we're willing to replicate.
		for _, repoID := range n.Config.Node.ReplicateRepos {
			log.Debugf("[content announce] announcing repo '%v'", repoID)

			err := n.announceRepoReplicator(ctx, repoID)
			if err != nil {
				log.Warnf("[content announce] %+v", err)
				continue
			}
		}

		// Announce the repos we have locally
		err := n.repoManager.ForEachRepo(func(r *repo.Repo) error {
			repoID, err := r.RepoID()
			if err != nil {
				return err
			}

			err = n.AnnounceRepo(ctx, repoID)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			log.Warnf("[content announce] %+v", err)
		}

		time.Sleep(time.Duration(n.Config.Node.ContentAnnounceInterval))
	}
}

// This method is called via the RPC connection when a user git-pushes new content to the network.
// A push is actually a request to be pulled from, and in order for peers to pull from us, they need
// to know that we have the content in question.  The content is new, and therefore hasn't been
// announced before; hence, the reason for this.
func (n *Node) AnnounceRepoContent(ctx context.Context, repoID string) error {
	repo := n.repoManager.Repo(repoID)
	if repo == nil {
		return errors.Errorf("repo '%v' not found", repoID)
	}

	err := n.AnnounceRepo(ctx, repoID)
	if err != nil {
		return err
	}

	return nil
}

// Announce to the swarm that this Node can provide objects from the given repository.
func (n *Node) AnnounceRepo(ctx context.Context, repoID string) error {
	c, err := util.CidForString(repoID)
	if err != nil {
		return err
	}

	err = n.dht.Provide(ctx, c, true)
	if err != nil && err != kbucket.ErrLookupFailure {
		return errors.Wrapf(err, "could not dht.Provide repo '%v'", repoID)
	}
	return nil
}

// Announce to the swarm that this Node is willing to replicate objects from the given repository.
func (n *Node) announceRepoReplicator(ctx context.Context, repoID string) error {
	c, err := util.CidForString("replicate:" + repoID)
	if err != nil {
		return err
	}

	err = n.dht.Provide(ctx, c, true)
	if err != nil && err != kbucket.ErrLookupFailure {
		return errors.Wrapf(err, "could not dht.Provide replicator for repo '%v'", repoID)
	}
	return nil
}

// Adds a peer to the Node's address book and attempts to .Connect to it using the libp2p Host.
func (n *Node) AddPeer(ctx context.Context, multiaddrString string) error {
	// The following code extracts the peer ID from the given multiaddress
	addr, err := ma.NewMultiaddr(multiaddrString)
	if err != nil {
		return errors.Wrapf(err, "could not parse multiaddr '%v'", multiaddrString)
	}

	pinfo, err := pstore.InfoFromP2pAddr(addr)
	if err != nil {
		return errors.Wrapf(err, "could not parse PeerInfo from multiaddr '%v'", multiaddrString)
	}

	err = n.host.Connect(ctx, *pinfo)
	if err != nil {
		return errors.Wrapf(err, "could not connect to peer '%v'", multiaddrString)
	}
	return nil
}

func (n *Node) RemovePeer(peerID peer.ID) error {
	if len(n.host.Network().ConnsToPeer(peerID)) > 0 {
		err := n.host.Network().ClosePeer(peerID)
		if err != nil {
			return err
		}
	}
	n.host.Peerstore().ClearAddrs(peerID)
	return nil
}

func (n *Node) Clone(ctx context.Context, opts *nodep2p.CloneOptions) (*repo.Repo, error) {
	return nodep2p.Clone(ctx, opts)
}

func (n *Node) Push(ctx context.Context, opts *nodep2p.PushOptions) (string, error) {
	repoRoot := opts.Repo.Path()
	repoID, err := opts.Repo.RepoID()
	if err != nil {
		return "", err
	}
	commit, err := nodep2p.Push(ctx, opts)
	if err != nil {
		return "", err
	}
	event := MaybeEvent{
		EventType: PushedRepo,
		PushedRepoEvent: &PushedRepoEvent{
			RepoID:     repoID,
			RepoRoot:   repoRoot,
			BranchName: opts.BranchName,
			Commit:     commit,
		},
	}
	n.NotifyWatchers(event)
	return commit, nil
}

func (n *Node) FetchConscienceRemote(ctx context.Context, opts *nodep2p.FetchOptions) ([]string, error) {
	repoRoot := opts.Repo.Path()
	repoID, err := opts.Repo.RepoID()
	if err != nil {
		return nil, err
	}
	updatedRefs, err := nodep2p.FetchConscienceRemote(ctx, opts)
	if err != nil {
		return nil, err
	}
	event := MaybeEvent{
		EventType: PulledRepo,
		PulledRepoEvent: &PulledRepoEvent{
			RepoID:      repoID,
			RepoRoot:    repoRoot,
			UpdatedRefs: updatedRefs,
		},
	}
	n.NotifyWatchers(event)
	return updatedRefs, nil
}

func (n *Node) Pull(ctx context.Context, opts *nodep2p.PullOptions) ([]string, error) {
	repoRoot := opts.Repo.Path()
	repoID, err := opts.Repo.RepoID()
	if err != nil {
		return nil, err
	}
	updatedRefs, err := nodep2p.Pull(ctx, opts)
	if err != nil {
		return nil, err
	}
	event := MaybeEvent{
		EventType: PulledRepo,
		PulledRepoEvent: &PulledRepoEvent{
			RepoID:      repoID,
			RepoRoot:    repoRoot,
			UpdatedRefs: updatedRefs,
		},
	}
	n.NotifyWatchers(event)
	return updatedRefs, nil
}

func (n *Node) FetchFromCommit(ctx context.Context, repoID string, repoPath string, commit git.Oid, checkoutType CheckoutType) (<-chan nodep2p.MaybeFetchFromCommitPacket, int64, int64) {
	c := nodep2p.NewClient(n, repoID, repoPath, &n.Config)
	return c.FetchFromCommit(ctx, commit, checkoutType)
}

func (n *Node) FetchChunks(ctx context.Context, repoID string, repoPath string, chunkObjects [][]byte) <-chan nodep2p.MaybeChunk {
	c := nodep2p.NewClient(n, repoID, repoPath, &n.Config)
	return c.FetchChunks(ctx, chunkObjects)
}

func (n *Node) RequestBecomeReplicator(ctx context.Context, repoID string) error {
	return nodep2p.RequestBecomeReplicator(ctx, n, repoID)
}

func (n *Node) RequestReplication(ctx context.Context, repoID string) <-chan Progress {
	return nodep2p.RequestReplication(ctx, n, repoID)
}

func (n *Node) SetReplicationPolicy(repoID string, shouldReplicate bool) error {
	return n.Config.Update(func() error {
		if shouldReplicate {
			n.Config.Node.ReplicateRepos = util.StringSetAdd(n.Config.Node.ReplicateRepos, repoID)
		} else {
			n.Config.Node.ReplicateRepos = util.StringSetRemove(n.Config.Node.ReplicateRepos, repoID)
		}
		return nil
	})
}

func (n *Node) GetConfig() config.Config {
	return n.Config
}

func (n *Node) RepoManager() *RepoManager {
	return n.repoManager
}

func (n *Node) Repo(repoID string) *repo.Repo {
	return n.repoManager.Repo(repoID)
}

func (n *Node) RepoAtPath(path string) *repo.Repo {
	return n.repoManager.RepoAtPath(path)
}

func (n *Node) RepoAtPathOrID(path string, repoID string) (*repo.Repo, error) {
	return n.repoManager.RepoAtPathOrID(path, repoID)
}

func (n *Node) TrackRepo(repoPath string, forceReload bool) (*repo.Repo, error) {
	r, err := n.repoManager.TrackRepo(repoPath, forceReload)
	if err != nil {
		return nil, err
	}
	repoID, err := r.RepoID()
	if err != nil {
		return nil, err
	}
	event := MaybeEvent{
		EventType: AddedRepo,
		AddedRepoEvent: &AddedRepoEvent{
			RepoRoot: repoPath,
			RepoID:   repoID,
		},
	}
	n.NotifyWatchers(event)
	return r, nil
}

func (n *Node) ID() peer.ID {
	return n.host.ID()
}

func (n *Node) Addrs() []ma.Multiaddr {
	return n.host.Addrs()
}

func (n *Node) NewStream(ctx context.Context, peerID peer.ID, pids ...protocol.ID) (netp2p.Stream, error) {
	return n.host.NewStream(ctx, peerID, pids...)
}

func (n *Node) FindProvidersAsync(ctx context.Context, key cid.Cid, count int) <-chan pstore.PeerInfo {
	return n.dht.FindProvidersAsync(ctx, key, count)
}

func (n *Node) Peers() []pstore.PeerInfo {
	return pstore.PeerInfos(n.host.Peerstore(), n.host.Peerstore().Peers())
}

func (n *Node) Conns() []netp2p.Conn {
	return n.host.Network().Conns()
}

func (n *Node) EthAddress() nodeeth.Address {
	return n.eth.Address()
}

func (n *Node) GetUsername(ctx context.Context) (string, error) {
	return n.eth.GetUsername(ctx)
}

func (n *Node) EnsureUsername(ctx context.Context, username string) (*nodeeth.Transaction, error) {
	return n.eth.EnsureUsername(ctx, username)
}

func (n *Node) EnsureRepoIDRegistered(ctx context.Context, repoID string) (*nodeeth.Transaction, error) {
	return n.eth.EnsureRepoIDRegistered(ctx, repoID)
}

func (n *Node) AddrFromSignedHash(data, sig []byte) (nodeeth.Address, error) {
	return n.eth.AddrFromSignedHash(data, sig)
}

func (n *Node) AddressHasPullAccess(ctx context.Context, user nodeeth.Address, repoID string) (bool, error) {
	return n.eth.AddressHasPullAccess(ctx, user, repoID)
}

func (n *Node) IsBehindRemote(ctx context.Context, repoID string, path string) (bool, error) {
	r, err := n.repoManager.RepoAtPathOrID(path, repoID)
	if err != nil {
		return false, err
	}

	remote, err := n.eth.GetRef(ctx, repoID, "refs/heads/master")
	if err != nil {
		return false, err
	}

	if len(remote) == 0 {
		return false, nil
	}

	remoteHash, err := hex.DecodeString(remote)
	if err != nil {
		return false, err
	}
	return !r.HasObject(remoteHash), nil
}

func (n *Node) GetNumRefs(ctx context.Context, repoID string) (uint64, error) {
	return n.eth.GetNumRefs(ctx, repoID)
}

func (n *Node) GetRemoteRefs(ctx context.Context, repoID string, pageSize uint64, page uint64) (map[string]Ref, uint64, error) {
	return n.eth.GetRefs(ctx, repoID, pageSize, page)
}

func (n *Node) ForEachRemoteRef(ctx context.Context, repoID string, fn func(Ref) (bool, error)) error {
	var page uint64
	var scanned uint64
	var total uint64
	var err error
	var refmap map[string]Ref

	for {
		refmap, total, err = n.GetRemoteRefs(ctx, repoID, 10, page)
		if err != nil {
			return err
		}

		for _, ref := range refmap {
			proceed, err := fn(ref)
			if err != nil {
				return err
			} else if !proceed {
				return nil
			}
			scanned++
		}

		if scanned >= total {
			break
		}
		page++
	}
	return nil
}

func (n *Node) UpdateRef(ctx context.Context, repoID string, refName string, commitHash string) (*nodeeth.Transaction, error) {
	return n.eth.UpdateRef(ctx, repoID, refName, commitHash)
}

func (n *Node) SetRepoPublic(ctx context.Context, repoID string, isPublic bool) (*nodeeth.Transaction, error) {
	return n.eth.SetRepoPublic(ctx, repoID, isPublic)
}

func (n *Node) IsRepoPublic(ctx context.Context, repoID string) (bool, error) {
	return n.eth.IsRepoPublic(ctx, repoID)
}

func (n *Node) GetRepoUsers(ctx context.Context, repoID string, userType nodeeth.UserType, pageSize uint64, page uint64) ([]string, uint64, error) {
	return n.eth.GetRepoUsers(ctx, repoID, userType, pageSize, page)
}

func (n *Node) GetUpdatedRefEvents(ctx context.Context, repoIDs []string, start uint64, end *uint64) ([]nodeeth.UpdatedRefEvent, error) {
	return n.eth.GetUpdatedRefEvents(ctx, repoIDs, start, end)
}

func (n *Node) SignHash(data []byte) ([]byte, error) {
	return n.eth.SignHash(data)
}

func (n *Node) SetUserPermissions(ctx context.Context, repoID string, username string, perms nodeeth.UserPermissions) (*nodeeth.Transaction, error) {
	return n.eth.SetUserPermissions(ctx, repoID, username, perms)
}

func (n *Node) CurrentBlock(ctx context.Context) (uint64, error) {
	return n.eth.CurrentBlock(ctx)
}

func (n *Node) GetBandwidthForPeer(p peer.ID) metrics.Stats {
	return n.bandwidthCounter.GetBandwidthForPeer(p)
}

func (n *Node) GetBandwidthForProtocol(proto protocol.ID) metrics.Stats {
	return n.bandwidthCounter.GetBandwidthForProtocol(proto)
}

func (n *Node) GetBandwidthTotals() metrics.Stats {
	return n.bandwidthCounter.GetBandwidthTotals()
}

func (n *Node) Watch(ctx context.Context, settings *WatcherSettings) *Watcher {
	w := NewWatcher(ctx, settings)
	n.watchers = append(n.watchers, w)

	if w.IsWatching(UpdatedRef) {
		repoIDs := n.repoManager.RepoIDList()
		rw := n.eth.WatchUpdatedRefEvents(ctx, repoIDs, settings.UpdatedRefStart)
		go w.AddUpdatedRefEventWatcher(rw)
	}

	return w
}

func (n *Node) NotifyWatchers(event MaybeEvent) {
	for _, w := range n.watchers {
		if w.IsWatching(event.EventType) {
			go w.Notify(event)
		}
	}
}
