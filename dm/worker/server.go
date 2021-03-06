// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package worker

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pingcap/dm/pkg/log"
	"github.com/pingcap/errors"
	"github.com/siddontang/go/sync2"
	"github.com/soheilhy/cmux"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/pingcap/dm/dm/common"
	"github.com/pingcap/dm/dm/config"
	"github.com/pingcap/dm/dm/pb"
)

var (
	cmuxReadTimeout = 10 * time.Second
)

// Server accepts RPC requests
// dispatches requests to worker
// sends responses to RPC client
type Server struct {
	sync.Mutex
	wg     sync.WaitGroup
	closed sync2.AtomicBool

	cfg *Config

	rootLis net.Listener
	svr     *grpc.Server
	worker  *Worker
}

// NewServer creates a new Server
func NewServer(cfg *Config) *Server {
	s := Server{
		cfg:    cfg,
		worker: NewWorker(cfg),
	}
	s.closed.Set(true) // not start yet
	return &s
}

// Start starts to serving
func (s *Server) Start() error {
	var err error
	s.rootLis, err = net.Listen("tcp", s.cfg.WorkerAddr)
	if err != nil {
		return errors.Trace(err)
	}

	err = s.worker.Init()
	if err != nil {
		return errors.Trace(err)
	}

	s.closed.Set(false)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// start running worker to handle requests
		s.worker.Start()
	}()

	// create a cmux
	m := cmux.New(s.rootLis)
	m.SetReadTimeout(cmuxReadTimeout) // set a timeout, ref: https://github.com/pingcap/tidb-binlog/pull/352

	// match connections in order: first gRPC, then HTTP
	grpcL := m.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
	httpL := m.Match(cmux.HTTP1Fast())

	s.svr = grpc.NewServer()
	pb.RegisterWorkerServer(s.svr, s)
	go func() {
		err2 := s.svr.Serve(grpcL)
		if err != nil && !common.IsErrNetClosing(err2) && err != cmux.ErrListenerClosed {
			log.Errorf("[server] gRPC server return with error %s", err.Error())
		}
	}()
	go InitStatus(httpL) // serve status

	log.Infof("[server] listening on %v for gRPC API and status request", s.cfg.WorkerAddr)
	err = m.Serve()
	if err != nil && common.IsErrNetClosing(err) {
		err = nil
	}
	return errors.Trace(err)
}

// Close close the RPC server
func (s *Server) Close() {
	s.Lock()
	defer s.Unlock()
	if s.closed.Get() {
		return
	}

	err := s.rootLis.Close()
	if err != nil && !common.IsErrNetClosing(err) {
		log.Errorf("[server] close net listener with error %s", err.Error())
	}
	if s.svr != nil {
		// GracefulStop can not cancel active stream RPCs
		// and the stream RPC may block on Recv or Send
		// so we use Stop instead to cancel all active RPCs
		s.svr.Stop()
	}

	// close worker and wait for return
	s.worker.Close()
	s.wg.Wait()

	s.closed.Set(true)
}

// StartSubTask implements WorkerServer.StartSubTask
func (s *Server) StartSubTask(ctx context.Context, req *pb.StartSubTaskRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive StartSubTask request %+v", req)

	cfg := config.NewSubTaskConfig()
	err := cfg.Decode(req.Task)
	if err != nil {
		log.Errorf("[server] decode config from request %+v error %v", req.Task, errors.ErrorStack(err))
		return &pb.CommonWorkerResponse{
			Result: false,
			Msg:    errors.ErrorStack(err),
		}, nil
	}

	err = s.worker.StartSubTask(cfg)
	if err != nil {
		log.Errorf("[server] start sub task %s error %v", cfg.Name, errors.ErrorStack(err))
		return &pb.CommonWorkerResponse{
			Result: false,
			Msg:    errors.ErrorStack(err),
		}, nil
	}

	return &pb.CommonWorkerResponse{
		Result: true,
		Msg:    "",
	}, nil
}

// OperateSubTask implements WorkerServer.OperateSubTask
func (s *Server) OperateSubTask(ctx context.Context, req *pb.OperateSubTaskRequest) (*pb.OperateSubTaskResponse, error) {
	log.Infof("[server] receive OperateSubTask request %+v", req)

	resp := &pb.OperateSubTaskResponse{
		Op:     req.Op,
		Result: false,
	}

	name := req.Name
	var err error
	switch req.Op {
	case pb.TaskOp_Stop:
		err = s.worker.StopSubTask(name)
	case pb.TaskOp_Pause:
		err = s.worker.PauseSubTask(name)
	case pb.TaskOp_Resume:
		err = s.worker.ResumeSubTask(name)
	default:
		resp.Msg = fmt.Sprintf("invalid operate %s on sub task", req.Op.String())
		return resp, nil
	}

	if err != nil {
		log.Errorf("[server] operate(%s) sub task %s error %v", req.Op.String(), name, errors.ErrorStack(err))
		resp.Msg = errors.ErrorStack(err)
		return resp, nil
	}

	resp.Result = true
	return resp, nil
}

// UpdateSubTask implements WorkerServer.UpdateSubTask
func (s *Server) UpdateSubTask(ctx context.Context, req *pb.UpdateSubTaskRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive UpdateSubTask request %+v", req)

	cfg := config.NewSubTaskConfig()
	err := cfg.Decode(req.Task)
	if err != nil {
		log.Errorf("[server] decode config from request %+v error %v", req.Task, errors.ErrorStack(err))
		return &pb.CommonWorkerResponse{
			Result: false,
			Msg:    errors.ErrorStack(err),
		}, nil
	}

	err = s.worker.UpdateSubTask(cfg)
	if err != nil {
		log.Errorf("[server] update sub task %s error %v", cfg.Name, errors.ErrorStack(err))
		return &pb.CommonWorkerResponse{
			Result: false,
			Msg:    errors.ErrorStack(err),
		}, nil
	}

	return &pb.CommonWorkerResponse{
		Result: true,
		Msg:    "",
	}, nil
}

// QueryStatus implements WorkerServer.QueryStatus
func (s *Server) QueryStatus(ctx context.Context, req *pb.QueryStatusRequest) (*pb.QueryStatusResponse, error) {
	log.Infof("[server] receive QueryStatus request %+v", req)

	resp := &pb.QueryStatusResponse{
		Result:        true,
		SubTaskStatus: s.worker.QueryStatus(req.Name),
		RelayStatus:   s.worker.relayHolder.Status(),
	}

	if len(resp.SubTaskStatus) == 0 {
		resp.Msg = "no sub task started"
	}
	return resp, nil
}

// QueryError implements WorkerServer.QueryError
func (s *Server) QueryError(ctx context.Context, req *pb.QueryErrorRequest) (*pb.QueryErrorResponse, error) {
	log.Infof("[server] receive QueryError request %+v", req)

	resp := &pb.QueryErrorResponse{
		Result:       true,
		SubTaskError: s.worker.QueryError(req.Name),
		RelayError:   s.worker.relayHolder.Error(),
	}

	return resp, nil
}

// FetchDDLInfo implements WorkerServer.FetchDDLInfo
// we do ping-pong send-receive on stream for DDL (lock) info
// if error occurred in Send / Recv, just retry in client
func (s *Server) FetchDDLInfo(stream pb.Worker_FetchDDLInfoServer) error {
	log.Infof("[server] receive FetchDDLInfo request")
	var ddlInfo *pb.DDLInfo
	for {
		// try fetch pending to sync DDL info from worker
		ddlInfo = s.worker.FetchDDLInfo(stream.Context())
		if ddlInfo == nil {
			return nil // worker closed or context canceled
		}
		log.Infof("[server] fetched DDLInfo from worker %v", ddlInfo)
		// send DDLInfo to dm-master
		err := stream.Send(ddlInfo)
		if err != nil {
			log.Errorf("[server] send DDLInfo %v to RPC stream fail %v", ddlInfo, err)
			return err
		}

		// receive DDLLockInfo from dm-master
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Errorf("[server] receive DDLLockInfo from RPC stream fail %v", err)
			return err
		}
		log.Infof("[server] receive DDLLockInfo %v", in)

		//ddlInfo = nil // clear and protect to put it back

		err = s.worker.RecordDDLLockInfo(in)
		if err != nil {
			// if error occurred when recording DDLLockInfo, log an error
			// user can handle this case using dmctl
			log.Errorf("[server] record DDLLockInfo %v to worker fail %v", in, errors.ErrorStack(err))
		}
	}
}

// ExecuteDDL implements WorkerServer.ExecuteDDL
func (s *Server) ExecuteDDL(ctx context.Context, req *pb.ExecDDLRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive ExecuteDDL request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: true,
	}
	err := s.worker.ExecuteDDL(ctx, req)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[server] %v ExecuteDDL error %v", req, errors.ErrorStack(err))
	}
	return resp, nil
}

// BreakDDLLock implements WorkerServer.BreakDDLLock
func (s *Server) BreakDDLLock(ctx context.Context, req *pb.BreakDDLLockRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive BreakDDLLock request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: true,
	}
	err := s.worker.BreakDDLLock(ctx, req)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[server] %v BreakDDLLock error %v", req, errors.ErrorStack(err))
	}
	return resp, nil
}

// HandleSQLs implements WorkerServer.HandleSQLs
func (s *Server) HandleSQLs(ctx context.Context, req *pb.HandleSubTaskSQLsRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive HandleSQLs request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: false,
		Msg:    "",
	}

	err := s.worker.HandleSQLs(ctx, req)
	if err != nil {
		log.Errorf("[server] handle sqls %+v error %v", req, errors.ErrorStack(err))
		resp.Msg = errors.ErrorStack(err)
		return resp, nil
	}

	resp.Result = true
	return resp, nil
}

// SwitchRelayMaster implements WorkerServer.SwitchRelayMaster
func (s *Server) SwitchRelayMaster(ctx context.Context, req *pb.SwitchRelayMasterRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive SwitchRelayMaster request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: true,
	}
	err := s.worker.SwitchRelayMaster(ctx, req)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[server] %v SwitchRelayMaster error %v", req, errors.ErrorStack(err))
	}

	return resp, nil
}

// OperateRelay implements WorkerServer.OperateRelay
func (s *Server) OperateRelay(ctx context.Context, req *pb.OperateRelayRequest) (*pb.OperateRelayResponse, error) {
	log.Infof("[server] receive OperateRelay request %+v", req)

	resp := &pb.OperateRelayResponse{
		Op:     req.Op,
		Result: false,
	}

	err := s.worker.OperateRelay(ctx, req)
	if err != nil {
		log.Errorf("[server] operate(%s) relay unit error %v", req.Op.String(), errors.ErrorStack(err))
		resp.Msg = errors.ErrorStack(err)
		return resp, nil
	}

	resp.Result = true
	return resp, nil
}

// PurgeRelay implements WorkerServer.PurgeRelay
func (s *Server) PurgeRelay(ctx context.Context, req *pb.PurgeRelayRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive PurgeRelay request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: true,
	}
	err := s.worker.PurgeRelay(ctx, req)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[server] %v PurgeRelay error %v", req, errors.ErrorStack(err))
	}

	return resp, nil
}

// UpdateRelayConfig updates config for relay and (dm-worker)
func (s *Server) UpdateRelayConfig(ctx context.Context, req *pb.UpdateRelayRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive UpdateRelayConfig request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: true,
		Msg:    "",
	}
	err := s.worker.UpdateRelayConfig(ctx, req.Content)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[server] %v UpdateRelayConfig error %v", req, errors.ErrorStack(err))
	}

	return resp, nil
}

// QueryWorkerConfig return worker config
// worker config is defined in worker directory now,
// to avoid circular import, we only return db config
func (s *Server) QueryWorkerConfig(ctx context.Context, req *pb.QueryWorkerConfigRequest) (*pb.QueryWorkerConfigResponse, error) {
	log.Infof("[server] receive query worker config request %+v", req)
	resp := &pb.QueryWorkerConfigResponse{
		Result: true,
	}

	workerCfg, err := s.worker.QueryConfig(ctx)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[worker] query worker config error %v", errors.ErrorStack(err))
		return resp, nil
	}

	rawConfig, err := workerCfg.From.Toml()
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[worker] marshal worker config error %v", errors.ErrorStack(err))
	}

	resp.Content = string(rawConfig)
	resp.SourceID = workerCfg.SourceID
	return resp, nil
}

// MigrateRelay migrate relay to original binlog pos
func (s *Server) MigrateRelay(ctx context.Context, req *pb.MigrateRelayRequest) (*pb.CommonWorkerResponse, error) {
	log.Infof("[server] receive MigrateRelay request %+v", req)

	resp := &pb.CommonWorkerResponse{
		Result: true,
		Msg:    "",
	}
	err := s.worker.MigrateRelay(ctx, req.BinlogName, req.BinlogPos)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.Errorf("[worker] %v MigrateRelay error %v", req, errors.ErrorStack(err))
	}
	return resp, nil
}
