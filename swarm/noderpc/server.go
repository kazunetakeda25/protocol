package noderpc

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"gopkg.in/src-d/go-git.v4"
	gitobject "gopkg.in/src-d/go-git.v4/plumbing/object"

	"github.com/Conscience/protocol/log"
	"github.com/Conscience/protocol/repo"
	"github.com/Conscience/protocol/swarm"
	"github.com/Conscience/protocol/swarm/nodeeth"
	"github.com/Conscience/protocol/swarm/nodegit"
	"github.com/Conscience/protocol/swarm/noderpc/pb"
	"github.com/Conscience/protocol/util"
)

type Server struct {
	node   *swarm.Node
	server *grpc.Server
}

func NewServer(node *swarm.Node) *Server {
	return &Server{node: node}
}

func (s *Server) Start() {
	lis, err := net.Listen(s.node.Config.Node.RPCListenNetwork, s.node.Config.Node.RPCListenHost)
	if err != nil {
		panic(fmt.Sprintf("failed to listen: %v\n", err))
	}

	var opts []grpc.ServerOption = []grpc.ServerOption{
		grpc.StreamInterceptor(StreamServerInterceptor()),
		grpc.UnaryInterceptor(UnaryServerInterceptor()),
	}
	s.server = grpc.NewServer(opts...)
	pb.RegisterNodeRPCServer(s.server, s)
	s.server.Serve(lis)
}

func (s *Server) Close() error {
	// This closes the net.Listener as well.
	s.server.GracefulStop()
	return nil
}

func (s *Server) SetUsername(ctx context.Context, req *pb.SetUsernameRequest) (*pb.SetUsernameResponse, error) {
	tx, err := s.node.EnsureUsername(ctx, req.Username)
	if err != nil {
		return nil, err
	}

	if tx != nil {
		resp := <-tx.Await(ctx)
		if resp.Err != nil {
			return nil, resp.Err
		} else if resp.Receipt.Status != 1 {
			return nil, errors.New("transaction failed")
		}
	}
	signature, err := s.node.SignHash([]byte(req.Username))
	if err != nil {
		return nil, err
	}
	return &pb.SetUsernameResponse{Signature: signature}, nil
}

func (s *Server) GetUsername(ctx context.Context, req *pb.GetUsernameRequest) (*pb.GetUsernameResponse, error) {
	un, err := s.node.GetUsername(ctx)
	if err != nil {
		return nil, err
	}
	signature, err := s.node.SignHash([]byte(un))
	if err != nil {
		return nil, err
	}

	return &pb.GetUsernameResponse{Username: un, Signature: signature}, nil
}

func (s *Server) InitRepo(ctx context.Context, req *pb.InitRepoRequest) (*pb.InitRepoResponse, error) {
	if req.RepoID == "" {
		return nil, errors.New("empty repoID")
	}

	// Before anything else, try to claim the repoID in the smart contract
	tx, err := s.node.EnsureRepoIDRegistered(ctx, req.RepoID)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if tx != nil {
		log.Printf("[rpc] create repo tx sent: %s", tx.Hash().Hex())
		txResult := <-tx.Await(ctx)
		if txResult.Err != nil {
			return nil, errors.WithStack(txResult.Err)
		}
		log.Printf("[rpc] create repo tx resolved: %s", tx.Hash().Hex())
	}

	err = s.node.RequestBecomeReplicator(ctx, req.RepoID)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// If no path was specified, create the repo in the ReplicationRoot
	path := req.Path
	if path == "" {
		path = filepath.Join(s.node.Config.Node.ReplicationRoot, req.RepoID)
	}

	// Open or create the git repo
	r, err := repo.Open(path)
	if err != nil {
		r, err = repo.Init(path)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	// Setup the Conscience plugins, etc.
	err = r.SetupConfig(req.RepoID)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	err = r.AddUserToConfig(req.Name, req.Email)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Have the node track the local repo
	_, err = s.node.RepoManager().TrackRepo(path, true)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// if local HEAD exists, push to contract
	_, err = r.Head()
	if err == nil {
		err = util.ExecAndScanStdout(ctx, []string{"git", "push", "origin", "master"}, req.Path, func(line string) error {
			return nil
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return &pb.InitRepoResponse{Path: path}, nil
}

func (s *Server) CheckpointRepo(ctx context.Context, req *pb.CheckpointRepoRequest) (*pb.CheckpointRepoResponse, error) {
	err := util.ExecAndScanStdout(ctx, []string{"git", "add", "."}, req.Path, func(line string) error {
		log.Debugln("[checkpoint]  -", line)
		return nil
	})
	if err != nil {
		log.Errorln("[checkpoint]  - error:", err)
		return nil, errors.WithStack(err)
	}

	err = util.ExecAndScanStdout(ctx, []string{"git", "commit", "-m", req.Message}, req.Path, func(line string) error {
		log.Debugln("[checkpoint]  -", line)
		return nil
	})
	if err != nil {
		log.Errorln("[checkpoint]  - error:", err)
		return nil, errors.WithStack(err)
	}

	err = util.ExecAndScanStdout(ctx, []string{"git", "push", "origin", "master"}, req.Path, func(line string) error {
		log.Debugln("[checkpoint]  -", line)
		return nil
	})
	if err != nil {
		log.Errorln("[checkpoint]  - error:", err)
		return nil, errors.WithStack(err)
	}

	return &pb.CheckpointRepoResponse{Ok: true}, nil
}

func (s *Server) PullRepo(req *pb.PullRepoRequest, server pb.NodeRPC_PullRepoServer) error {
	// @@TODO: give context a timeout and make it configurable
	ctx := context.Background()
	ch := nodegit.PullRepo(ctx, req.Path)

	for progress := range ch {
		if progress.Error != nil {
			return errors.WithStack(progress.Error)
		}
		err := server.Send(&pb.PullRepoResponsePacket{
			ToFetch: progress.ToFetch,
			Fetched: progress.Fetched,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func (s *Server) CloneRepo(req *pb.CloneRepoRequest, server pb.NodeRPC_CloneRepoServer) error {
	repoRoot := req.Path
	if len(repoRoot) == 0 {
		repoRoot = s.node.Config.Node.ReplicationRoot
	}

	ctx := context.Background()
	ch := nodegit.CloneRepo(ctx, repoRoot, req.RepoID)

	for progress := range ch {
		if progress.Error != nil {
			return errors.WithStack(progress.Error)
		}

		err := server.Send(&pb.CloneRepoResponsePacket{
			Payload: &pb.CloneRepoResponsePacket_Progress_{&pb.CloneRepoResponsePacket_Progress{
				ToFetch: progress.ToFetch,
				Fetched: progress.Fetched,
			}},
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}

	repoFolder := req.RepoID
	if strings.Contains(repoFolder, "/") {
		parts := strings.Split(repoFolder, "/")
		repoFolder = parts[len(parts)-1]
	}

	r, err := repo.Open(filepath.Join(repoRoot, repoFolder))
	if err != nil {
		return errors.WithStack(err)
	}

	err = r.AddUserToConfig(req.Name, req.Email)
	if err != nil {
		return errors.WithStack(err)
	}

	err = server.Send(&pb.CloneRepoResponsePacket{
		Payload: &pb.CloneRepoResponsePacket_Success_{&pb.CloneRepoResponsePacket_Success{
			Path: r.Path,
		}},
	})
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (s *Server) FetchFromCommit(req *pb.FetchFromCommitRequest, server pb.NodeRPC_FetchFromCommitServer) error {
	// @@TODO: give context a timeout and make it configurable
	ch, uncompressedSize := s.node.FetchFromCommit(context.TODO(), req.RepoID, req.Path, util.GitHashFromBytes(req.Commit))

	err := server.Send(&pb.FetchFromCommitResponse{
		Payload: &pb.FetchFromCommitResponse_Header_{&pb.FetchFromCommitResponse_Header{
			UncompressedSize: uncompressedSize,
		}},
	})
	if err != nil {
		return errors.WithStack(err)
	}

	for pkt := range ch {
		select {
		case <-server.Context().Done():
			return errors.WithStack(server.Context().Err())
		default:
		}

		switch {
		case pkt.Error != nil:
			return errors.WithStack(pkt.Error)

		case pkt.PackfileHeader != nil:
			err = server.Send(&pb.FetchFromCommitResponse{
				Payload: &pb.FetchFromCommitResponse_PackfileHeader_{&pb.FetchFromCommitResponse_PackfileHeader{
					PackfileID:       pkt.PackfileHeader.PackfileID,
					UncompressedSize: pkt.PackfileHeader.UncompressedSize,
				}},
			})

		case pkt.PackfileData != nil:
			err = server.Send(&pb.FetchFromCommitResponse{
				Payload: &pb.FetchFromCommitResponse_PackfileData_{&pb.FetchFromCommitResponse_PackfileData{
					PackfileID: pkt.PackfileData.PackfileID,
					Data:       pkt.PackfileData.Data,
					End:        pkt.PackfileData.End,
				}},
			})
		}

		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func (s *Server) RegisterRepoID(ctx context.Context, req *pb.RegisterRepoIDRequest) (*pb.RegisterRepoIDResponse, error) {
	tx, err := s.node.EnsureRepoIDRegistered(ctx, req.RepoID)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if tx != nil {
		log.Printf("[rpc] create repo tx sent: %s", tx.Hash().Hex())
		txResult := <-tx.Await(ctx)
		if txResult.Err != nil {
			return nil, errors.WithStack(txResult.Err)
		}
		log.Printf("[rpc] create repo tx resolved: %s", tx.Hash().Hex())
	}
	return &pb.RegisterRepoIDResponse{}, nil
}

func (s *Server) TrackLocalRepo(ctx context.Context, req *pb.TrackLocalRepoRequest) (*pb.TrackLocalRepoResponse, error) {
	_, err := s.node.RepoManager().TrackRepo(req.RepoPath, req.ForceReload)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &pb.TrackLocalRepoResponse{}, nil
}

func (s *Server) GetLocalRepos(req *pb.GetLocalReposRequest, server pb.NodeRPC_GetLocalReposServer) error {
	return s.node.RepoManager().ForEachRepo(func(r *repo.Repo) error {
		select {
		case <-server.Context().Done():
			return errors.WithStack(server.Context().Err())
		default:
		}

		repoID, err := r.RepoID()
		if err != nil {
			return errors.WithStack(err)
		}
		err = server.Send(&pb.GetLocalReposResponsePacket{RepoID: repoID, Path: r.Path})
		return errors.WithStack(err)
	})
}

func (s *Server) SetReplicationPolicy(ctx context.Context, req *pb.SetReplicationPolicyRequest) (*pb.SetReplicationPolicyResponse, error) {
	err := s.node.SetReplicationPolicy(req.RepoID, req.ShouldReplicate)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &pb.SetReplicationPolicyResponse{}, nil
}

func (s *Server) AnnounceRepoContent(ctx context.Context, req *pb.AnnounceRepoContentRequest) (*pb.AnnounceRepoContentResponse, error) {
	err := s.node.AnnounceRepoContent(ctx, req.RepoID)
	if err != nil {
		return nil, err
	}
	return &pb.AnnounceRepoContentResponse{}, nil
}

func (s *Server) GetLocalRefs(ctx context.Context, req *pb.GetLocalRefsRequest) (*pb.GetLocalRefsResponse, error) {
	r, err := s.node.RepoManager().RepoAtPathOrID(req.Path, req.RepoID)
	if err != nil {
		return nil, err
	}

	rIter, err := r.References()
	if err != nil {
		return nil, err
	}
	defer rIter.Close()

	refs := []*pb.Ref{}
	for {
		ref, err := rIter.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		refs = append(refs, &pb.Ref{
			RefName:    ref.Name().String(),
			CommitHash: ref.Hash().String(),
		})
	}

	return &pb.GetLocalRefsResponse{Refs: refs}, nil
}

func (s *Server) GetRemoteRefs(ctx context.Context, req *pb.GetRemoteRefsRequest) (*pb.GetRemoteRefsResponse, error) {
	refMap, total, err := s.node.GetRemoteRefs(ctx, req.RepoID, req.PageSize, req.Page)
	if err != nil {
		return nil, err
	}

	refs := []*pb.Ref{}
	for _, ref := range refMap {
		refs = append(refs, &pb.Ref{RefName: ref.RefName, CommitHash: ref.CommitHash})
	}

	return &pb.GetRemoteRefsResponse{Total: total, Refs: refs}, nil
}

func (s *Server) IsBehindRemote(ctx context.Context, req *pb.IsBehindRemoteRequest) (*pb.IsBehindRemoteResponse, error) {
	isBehindRemote, err := s.node.IsBehindRemote(ctx, req.RepoID, req.Path)
	if err != nil {
		return nil, err
	}

	return &pb.IsBehindRemoteResponse{RepoID: req.RepoID, IsBehindRemote: isBehindRemote}, nil
}

func (s *Server) UpdateRef(ctx context.Context, req *pb.UpdateRefRequest) (*pb.UpdateRefResponse, error) {
	tx, err := s.node.UpdateRef(ctx, req.RepoID, req.RefName, req.CommitHash)
	if err != nil {
		return nil, err
	}

	txResult := <-tx.Await(ctx)
	if txResult.Err != nil {
		return nil, txResult.Err
	} else if txResult.Receipt.Status == 0 {
		return nil, errors.New("transaction failed")
	}

	return &pb.UpdateRefResponse{}, nil
}

func (s *Server) SetRepoPublic(ctx context.Context, req *pb.SetRepoPublicRequest) (*pb.SetRepoPublicResponse, error) {
	tx, err := s.node.SetRepoPublic(ctx, req.RepoID, req.IsPublic)
	if err != nil {
		return nil, err
	}
	txResult := <-tx.Await(ctx)
	if txResult.Err != nil {
		return nil, txResult.Err
	} else if txResult.Receipt.Status == 0 {
		return nil, errors.New("transaction failed")
	}

	return &pb.SetRepoPublicResponse{RepoID: req.RepoID, IsPublic: req.IsPublic}, nil
}

func (s *Server) IsRepoPublic(ctx context.Context, req *pb.IsRepoPublicRequest) (*pb.IsRepoPublicResponse, error) {
	isPublic, err := s.node.IsRepoPublic(ctx, req.RepoID)
	if err != nil {
		return nil, err
	}
	return &pb.IsRepoPublicResponse{RepoID: req.RepoID, IsPublic: isPublic}, nil
}

func (s *Server) GetRepoUsers(ctx context.Context, req *pb.GetRepoUsersRequest) (*pb.GetRepoUsersResponse, error) {
	users, total, err := s.node.GetRepoUsers(ctx, req.RepoID, nodeeth.UserType(req.Type), req.PageSize, req.Page)
	if err != nil {
		return nil, err
	}
	return &pb.GetRepoUsersResponse{Total: total, Users: users}, nil
}

func (s *Server) RequestReplication(req *pb.ReplicationRequest, server pb.NodeRPC_RequestReplicationServer) error {
	ch := s.node.RequestReplication(context.TODO(), req.RepoID)
	for progress := range ch {
		select {
		case <-server.Context().Done():
			return errors.WithStack(server.Context().Err())
		default:
		}

		if progress.Error != nil {
			return errors.WithStack(progress.Error)
		}
		err := server.Send(&pb.ReplicationResponsePacket{Percent: int32(progress.Percent)})
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func (s *Server) GetRepoHistory(ctx context.Context, req *pb.GetRepoHistoryRequest) (*pb.GetRepoHistoryResponse, error) {
	r, err := s.node.RepoManager().RepoAtPathOrID(req.Path, req.RepoID)
	if err != nil {
		return nil, err
	}

	// if HEAD does not exist, return empty commit list
	_, err = r.Head()
	if err != nil {
		return &pb.GetRepoHistoryResponse{Commits: []*pb.Commit{}}, nil
	}

	cIter, err := r.Log(&git.LogOptions{From: repo.ZeroHash, Order: git.LogOrderDFS})
	if err != nil {
		return nil, err
	}

	logs, err := s.node.GetRefLogs(ctx, req.RepoID)
	if err != nil {
		return nil, err
	}

	commits := []*pb.Commit{}
	err = cIter.ForEach(func(commit *gitobject.Commit) error {
		if commit == nil {
			log.Warnf("[node] nil commit (repoID: %v)", req.RepoID)
			return nil
		}
		commitHash := commit.Hash.String()
		files, err := nodegit.GetFilesForCommit(ctx, r.Path, commitHash)
		if err != nil {
			return err
		}
		verified := logs[commitHash]
		commits = append(commits, &pb.Commit{
			CommitHash: commitHash,
			Author:     commit.Author.String(),
			Message:    commit.Message,
			Timestamp:  uint64(commit.Author.When.Unix()),
			Files:      files,
			Verified:   verified,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &pb.GetRepoHistoryResponse{Commits: commits}, nil
}

func (s *Server) GetRepoFiles(ctx context.Context, req *pb.GetRepoFilesRequest) (*pb.GetRepoFilesResponse, error) {
	r, err := s.node.RepoManager().RepoAtPathOrID(req.RepoRoot, req.RepoID)
	if err != nil {
		return nil, err
	}

	fileList, err := r.ListFiles(ctx, repo.CommitID{Hash: util.GitHashFromBytes(req.CommitHash), Ref: req.CommitRef})
	if err != nil {
		return nil, err
	}

	files := make([]*pb.File, len(fileList))
	for i := range fileList {
		files[i] = &pb.File{
			Name:           fileList[i].Filename,
			Hash:           fileList[i].Hash[:],
			Mode:           uint32(fileList[i].Mode),
			Size:           fileList[i].Size,
			Modified:       fileList[i].Modified,
			UnstagedStatus: string(fileList[i].Status.Worktree),
			StagedStatus:   string(fileList[i].Status.Staging),
		}
	}

	return &pb.GetRepoFilesResponse{Files: files}, nil
}

func (s *Server) RepoHasObject(ctx context.Context, req *pb.RepoHasObjectRequest) (*pb.RepoHasObjectResponse, error) {
	r, err := s.node.RepoManager().RepoAtPathOrID(req.Path, req.RepoID)
	if err != nil {
		return nil, err
	}

	return &pb.RepoHasObjectResponse{HasObject: r.HasObject(req.ObjectID)}, nil
}

func (s *Server) SignMessage(ctx context.Context, req *pb.SignMessageRequest) (*pb.SignMessageResponse, error) {
	signature, err := s.node.SignHash(req.Message)
	if err != nil {
		return nil, err
	}
	return &pb.SignMessageResponse{Signature: signature}, nil
}

func (s *Server) EthAddress(ctx context.Context, req *pb.EthAddressRequest) (*pb.EthAddressResponse, error) {
	addr := s.node.EthAddress()
	return &pb.EthAddressResponse{Address: addr.String()}, nil
}

func (s *Server) SetUserPermissions(ctx context.Context, req *pb.SetUserPermissionsRequest) (*pb.SetUserPermissionsResponse, error) {
	tx, err := s.node.SetUserPermissions(ctx, req.RepoID, req.Username, nodeeth.UserPermissions{Puller: req.Puller, Pusher: req.Pusher, Admin: req.Admin})
	if err != nil {
		return nil, err
	}

	txResult := <-tx.Await(ctx)
	if txResult.Err != nil {
		return nil, txResult.Err
	} else if txResult.Receipt.Status == 0 {
		return nil, errors.New("transaction failed")
	}
	return &pb.SetUserPermissionsResponse{}, nil
}

// @@TODO: make configurable
const OBJ_CHUNK_SIZE = 512 * 1024 // 512kb

func (s *Server) GetObject(req *pb.GetObjectRequest, server pb.NodeRPC_GetObjectServer) error {
	r, err := s.node.RepoManager().RepoAtPathOrID(req.RepoRoot, req.RepoID)
	if err != nil {
		return err
	}

	var objectReader repo.ObjectReader

	if len(req.ObjectID) > 0 {
		objectReader, err = r.OpenObject(req.ObjectID)
		if err != nil {
			return err
		}

	} else {
		if len(req.CommitHash) != 20 && req.CommitRef == "" {
			return errors.New("need commitHash or commitRef")
		} else if len(req.Filename) == 0 {
			return errors.New("need filename")
		}

		if req.CommitRef == "working" {
			objectReader, err = r.OpenFileInWorktree(req.Filename)
			if err != nil {
				return err
			}

		} else {
			objectReader, err = r.OpenFileAtCommit(req.Filename, repo.CommitID{Hash: util.GitHashFromBytes(req.CommitHash), Ref: req.CommitRef})
			if err != nil {
				return err
			}
		}
	}
	defer objectReader.Close()

	err = server.Send(&pb.GetObjectResponse{
		Payload: &pb.GetObjectResponse_Header_{&pb.GetObjectResponse_Header{
			UncompressedSize: objectReader.Len(),
		}},
	})
	if err != nil {
		return err
	}

	totalBytes := req.MaxSize
	if totalBytes > objectReader.Len() {
		totalBytes = objectReader.Len()
	}

	var sent uint64
	for sent < totalBytes {
		bufSize := uint64(OBJ_CHUNK_SIZE)
		if sent+bufSize > totalBytes {
			bufSize = totalBytes - sent
		}

		data := make([]byte, bufSize)
		n, err := io.ReadFull(objectReader, data)
		if err == io.EOF {
			break
		} else if err == io.ErrUnexpectedEOF {
			data = data[:n]
		} else if err != nil {
			return err
		}

		sent += uint64(n)

		err = server.Send(&pb.GetObjectResponse{
			Payload: &pb.GetObjectResponse_Data_{&pb.GetObjectResponse_Data{
				Data: data,
			}},
		})
		if err != nil {
			return err
		}

		if sent == totalBytes {
			break
		}
	}

	err = server.Send(&pb.GetObjectResponse{
		Payload: &pb.GetObjectResponse_Data_{&pb.GetObjectResponse_Data{
			End: true,
		}},
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Server) GetDiff(req *pb.GetDiffRequest, server pb.NodeRPC_GetDiffServer) error {
	r, err := s.node.RepoManager().RepoAtPathOrID(req.RepoRoot, req.RepoID)
	if err != nil {
		return err
	}

	if len(req.CommitHash) != 20 && req.CommitRef == "" {
		return errors.New("need commitHash or commitRef")
	}

	changes, err := r.GetDiff(context.TODO(), repo.CommitID{Hash: util.GitHashFromBytes(req.CommitHash), Ref: req.CommitRef})
	if err != nil {
		return err
	}

	patch, err := changes.PatchContext(context.TODO())
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	go func() {
		var err error
		defer func() { pw.CloseWithError(err) }()
		err = patch.Encode(pw)
	}()

	for {
		data := make([]byte, OBJ_CHUNK_SIZE)
		n, err := io.ReadFull(pr, data)
		if err == io.EOF {
			break
		} else if err == io.ErrUnexpectedEOF {
			data = data[:n]
		} else if err != nil {
			return err
		}

		err = server.Send(&pb.GetDiffResponse{Data: data})
		if err != nil {
			return err
		}
	}

	err = server.Send(&pb.GetDiffResponse{End: true})
	if err != nil {
		return err
	}
	return nil
}
