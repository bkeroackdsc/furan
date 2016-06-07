package cmd

import (
	"fmt"
	"log"
	"net"

	"github.com/gocql/gocql"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type grpcserver struct {
}

var grpcServer grpcserver
var workerChan chan *workerRequest

type workerRequest struct {
	ctx context.Context
	req *BuildRequest
	id  gocql.UUID
}

func buildWorker() {
	var wreq *workerRequest
	for {
		wreq = <-workerChan
		if !isCancelled(wreq.ctx.Done()) {
			grpcServer.syncBuild(wreq.ctx, wreq.req, wreq.id)
		}
	}
}

func listenRPC() {
	addr := fmt.Sprintf("%v:%v", serverConfig.grpcAddr, serverConfig.grpcPort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("error starting gRPC listener: %v", err)
		return
	}
	s := grpc.NewServer()
	RegisterFuranExecutorServer(s, &grpcServer)
	log.Printf("gRPC listening on: %v", addr)
	s.Serve(l)
}

func (gr *grpcserver) finishBuild(id gocql.UUID, failed bool) error {
	flags := map[string]bool{
		"failed":   failed,
		"finished": true,
	}
	return setBuildFlags(dbConfig.session, id, flags)
}

// Performs build synchronously
func (gr *grpcserver) syncBuild(ctx context.Context, req *BuildRequest, id gocql.UUID) {
	ctx = context.WithValue(ctx, "id", id.String())
	builder, err := NewImageBuilder(gitConfig.token)
	if err != nil {
		gr.finishBuild(id, true)
		log.Printf("%v: error creating image builder: %v", id.String(), err)
		return
	}
	err = setBuildState(dbConfig.session, id, BuildStatusResponse_BUILDING)
	if err != nil {
		gr.finishBuild(id, true)
		log.Printf("error setting build state to building: %v", err)
		return
	}
	imageid, err := builder.Build(ctx, req, id)
	if err != nil {
		log.Printf("error performing build: %v", err)
		setBuildState(dbConfig.session, id, BuildStatusResponse_BUILD_FAILURE)
		gr.finishBuild(id, true)
		return
	}
	err = setBuildState(dbConfig.session, id, BuildStatusResponse_PUSHING)
	if err != nil {
		gr.finishBuild(id, true)
		log.Printf("error setting build state to pushing: %v", err)
		return
	}
	if req.Push.Registry.Repo == "" {
		err = builder.PushBuildToS3(ctx, req)
	} else {
		err = builder.PushBuildToRegistry(ctx, req)
	}
	if err != nil {
		gr.finishBuild(id, true)
		setBuildState(dbConfig.session, id, BuildStatusResponse_PUSH_FAILURE)
		log.Printf("error pushing: %v", err)
		return
	}
	err = builder.CleanImage(ctx, imageid)
	if err != nil {
		gr.finishBuild(id, true)
		setBuildState(dbConfig.session, id, BuildStatusResponse_PUSH_FAILURE)
		log.Printf("error cleaning built image: %v", err)
		return
	}
	err = setBuildState(dbConfig.session, id, BuildStatusResponse_SUCCESS)
	if err != nil {
		gr.finishBuild(id, true)
		log.Printf("error setting build state to success: %v", err)
		return
	}
	err = gr.finishBuild(id, false)
	if err != nil {
		log.Printf("error finalizing build: %v", err)
	}
	err = setBuildCompletedTimestamp(dbConfig.session, id)
	if err != nil {
		log.Printf("error setting build completed timestamp: %v", err)
	}
	log.Printf("build success for %v", id.String())
}

// gRPC handlers
func (gr *grpcserver) StartBuild(ctx context.Context, req *BuildRequest) (*BuildRequestResponse, error) {
	resp := &BuildRequestResponse{
		Error: &RPCError{},
	}
	if req.Push.Registry.Repo == "" {
		if req.Push.S3.Bucket == "" || req.Push.S3.KeyPrefix == "" || req.Push.S3.Region == "" {
			resp.Error.ErrorType = RPCError_BAD_REQUEST
			resp.Error.IsError = true
			resp.Error.ErrorMsg = "push registry and S3 configuration are both empty (at least one is required)"
			return resp, nil
		}
	}
	id, err := createBuild(dbConfig.session, req)
	if err != nil {
		resp.Error.ErrorType = RPCError_INTERNAL_ERROR
		resp.Error.IsError = true
		resp.Error.ErrorMsg = fmt.Sprintf("error creating build in DB: %v", err)
		return resp, nil
	}
	wreq := workerRequest{
		ctx: ctx,
		req: req,
		id:  *id,
	}
	select {
	case workerChan <- &wreq:
		resp.BuildId = id.String()
		return resp, nil
	default:
		err = deleteBuild(dbConfig.session, *id)
		if err != nil {
			log.Printf("error deleting build from DB: %v", err)
		}
		resp.Error.IsError = true
		resp.Error.ErrorType = RPCError_BAD_REQUEST
		resp.Error.ErrorMsg = "build queue is full; try again later"
		return resp, nil
	}
}

func (gr *grpcserver) GetBuildStatus(ctx context.Context, req *BuildStatusRequest) (*BuildStatusResponse, error) {
	resp := &BuildStatusResponse{
		Error: &RPCError{},
	}
	id, err := gocql.ParseUUID(req.BuildId)
	if err != nil {
		resp.Error.IsError = true
		resp.Error.ErrorMsg = fmt.Sprintf("bad id: %v", err)
		resp.Error.ErrorType = RPCError_BAD_REQUEST
		return resp, nil
	}
	resp, err = getBuildByID(dbConfig.session, id)
	if err != nil {
		if err == gocql.ErrNotFound {
			resp.Error.ErrorType = RPCError_BAD_REQUEST
		} else {
			resp.Error.ErrorType = RPCError_INTERNAL_ERROR
		}
		resp.Error.IsError = true
		resp.Error.ErrorMsg = fmt.Sprintf("error getting build: %v", err)
	}
	return resp, nil
}

func (gr *grpcserver) CancelBuild(ctx context.Context, req *BuildCancelRequest) (*BuildStatusResponse, error) {
	return &BuildStatusResponse{}, nil
}