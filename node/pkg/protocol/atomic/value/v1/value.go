// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	valuev1 "github.com/atomix/multi-raft-storage/api/atomix/multiraft/atomic/value/v1"
	"github.com/atomix/multi-raft-storage/node/pkg/protocol"
	"github.com/atomix/runtime/sdk/pkg/errors"
	"github.com/atomix/runtime/sdk/pkg/logging"
	streams "github.com/atomix/runtime/sdk/pkg/stream"
	"github.com/gogo/protobuf/proto"
)

var log = logging.GetLogger()

var atomicValueCodec = protocol.NewCodec[*valuev1.AtomicValueInput, *valuev1.AtomicValueOutput](
	func(input *valuev1.AtomicValueInput) ([]byte, error) {
		return proto.Marshal(input)
	},
	func(bytes []byte) (*valuev1.AtomicValueOutput, error) {
		output := &valuev1.AtomicValueOutput{}
		if err := proto.Unmarshal(bytes, output); err != nil {
			return nil, err
		}
		return output, nil
	})

func NewAtomicValueServer(node *protocol.Node) valuev1.AtomicValueServer {
	return &AtomicValueServer{
		protocol: protocol.NewProtocol[*valuev1.AtomicValueInput, *valuev1.AtomicValueOutput](node, atomicValueCodec),
	}
}

type AtomicValueServer struct {
	protocol protocol.Protocol[*valuev1.AtomicValueInput, *valuev1.AtomicValueOutput]
}

func (s *AtomicValueServer) Update(ctx context.Context, request *valuev1.UpdateRequest) (*valuev1.UpdateResponse, error) {
	log.Debugw("Update",
		logging.Stringer("UpdateRequest", request))
	input := &valuev1.AtomicValueInput{
		Input: &valuev1.AtomicValueInput_Update{
			Update: request.UpdateInput,
		},
	}
	output, headers, err := s.protocol.Command(ctx, input, request.Headers)
	if err != nil {
		err = errors.ToProto(err)
		log.Warnw("Update",
			logging.Stringer("UpdateRequest", request),
			logging.Error("Error", err))
		return nil, err
	}
	response := &valuev1.UpdateResponse{
		Headers:      headers,
		UpdateOutput: output.GetUpdate(),
	}
	log.Debugw("Update",
		logging.Stringer("UpdateRequest", request),
		logging.Stringer("UpdateResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Set(ctx context.Context, request *valuev1.SetRequest) (*valuev1.SetResponse, error) {
	log.Debugw("Set",
		logging.Stringer("SetRequest", request))
	input := &valuev1.AtomicValueInput{
		Input: &valuev1.AtomicValueInput_Set{
			Set: request.SetInput,
		},
	}
	output, headers, err := s.protocol.Command(ctx, input, request.Headers)
	if err != nil {
		err = errors.ToProto(err)
		log.Warnw("Set",
			logging.Stringer("SetRequest", request),
			logging.Error("Error", err))
		return nil, err
	}
	response := &valuev1.SetResponse{
		Headers:   headers,
		SetOutput: output.GetSet(),
	}
	log.Debugw("Set",
		logging.Stringer("SetRequest", request),
		logging.Stringer("SetResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Delete(ctx context.Context, request *valuev1.DeleteRequest) (*valuev1.DeleteResponse, error) {
	log.Debugw("Delete",
		logging.Stringer("DeleteRequest", request))
	input := &valuev1.AtomicValueInput{
		Input: &valuev1.AtomicValueInput_Delete{
			Delete: request.DeleteInput,
		},
	}
	output, headers, err := s.protocol.Command(ctx, input, request.Headers)
	if err != nil {
		err = errors.ToProto(err)
		log.Warnw("Delete",
			logging.Stringer("DeleteRequest", request),
			logging.Error("Error", err))
		return nil, err
	}
	response := &valuev1.DeleteResponse{
		Headers:      headers,
		DeleteOutput: output.GetDelete(),
	}
	log.Debugw("Delete",
		logging.Stringer("DeleteRequest", request),
		logging.Stringer("DeleteResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Get(ctx context.Context, request *valuev1.GetRequest) (*valuev1.GetResponse, error) {
	log.Debugw("Get",
		logging.Stringer("GetRequest", request))
	input := &valuev1.AtomicValueInput{
		Input: &valuev1.AtomicValueInput_Get{
			Get: request.GetInput,
		},
	}
	output, headers, err := s.protocol.Query(ctx, input, request.Headers)
	if err != nil {
		err = errors.ToProto(err)
		log.Warnw("Get",
			logging.Stringer("GetRequest", request),
			logging.Error("Error", err))
		return nil, err
	}
	response := &valuev1.GetResponse{
		Headers:   headers,
		GetOutput: output.GetGet(),
	}
	log.Debugw("Get",
		logging.Stringer("GetRequest", request),
		logging.Stringer("GetResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Events(request *valuev1.EventsRequest, server valuev1.AtomicValue_EventsServer) error {
	log.Debugw("Events",
		logging.Stringer("EventsRequest", request))
	input := &valuev1.AtomicValueInput{
		Input: &valuev1.AtomicValueInput_Events{
			Events: request.EventsInput,
		},
	}

	ch := make(chan streams.Result[*protocol.StreamCommandResponse[*valuev1.AtomicValueOutput]])
	stream := streams.NewChannelStream[*protocol.StreamCommandResponse[*valuev1.AtomicValueOutput]](ch)
	go func() {
		err := s.protocol.StreamCommand(server.Context(), input, request.Headers, stream)
		if err != nil {
			err = errors.ToProto(err)
			log.Warnw("Events",
				logging.Stringer("EventsRequest", request),
				logging.Error("Error", err))
			stream.Error(err)
			stream.Close()
		}
	}()

	for result := range ch {
		if result.Failed() {
			err := errors.ToProto(result.Error)
			log.Warnw("Events",
				logging.Stringer("EventsRequest", request),
				logging.Error("Error", err))
			return err
		}

		response := &valuev1.EventsResponse{
			Headers:      result.Value.Headers,
			EventsOutput: result.Value.Output.GetEvents(),
		}
		log.Debugw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Stringer("EventsResponse", response))
		if err := server.Send(response); err != nil {
			log.Warnw("Events",
				logging.Stringer("EventsRequest", request),
				logging.Error("Error", err))
			return err
		}
	}
	return nil
}

func (s *AtomicValueServer) Watch(request *valuev1.WatchRequest, server valuev1.AtomicValue_WatchServer) error {
	log.Debugw("Watch",
		logging.Stringer("WatchRequest", request))
	input := &valuev1.AtomicValueInput{
		Input: &valuev1.AtomicValueInput_Watch{
			Watch: request.WatchInput,
		},
	}

	ch := make(chan streams.Result[*protocol.StreamQueryResponse[*valuev1.AtomicValueOutput]])
	stream := streams.NewChannelStream[*protocol.StreamQueryResponse[*valuev1.AtomicValueOutput]](ch)
	go func() {
		err := s.protocol.StreamQuery(server.Context(), input, request.Headers, stream)
		if err != nil {
			err = errors.ToProto(err)
			log.Warnw("Watch",
				logging.Stringer("WatchRequest", request),
				logging.Error("Error", err))
			stream.Error(err)
			stream.Close()
		}
	}()

	for result := range ch {
		if result.Failed() {
			err := errors.ToProto(result.Error)
			log.Warnw("Watch",
				logging.Stringer("WatchRequest", request),
				logging.Error("Error", err))
			return err
		}

		response := &valuev1.WatchResponse{
			Headers:     result.Value.Headers,
			WatchOutput: result.Value.Output.GetWatch(),
		}
		log.Debugw("Watch",
			logging.Stringer("WatchRequest", request),
			logging.Stringer("WatchResponse", response))
		if err := server.Send(response); err != nil {
			log.Warnw("Watch",
				logging.Stringer("WatchRequest", request),
				logging.Error("Error", err))
			return err
		}
	}
	return nil
}

var _ valuev1.AtomicValueServer = (*AtomicValueServer)(nil)