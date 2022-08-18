// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	api "github.com/atomix/multi-raft-storage/api/atomix/multiraft/atomic/value/v1"
	multiraftv1 "github.com/atomix/multi-raft-storage/api/atomix/multiraft/v1"
	"github.com/atomix/multi-raft-storage/driver/pkg/client"
	valuev1 "github.com/atomix/runtime/api/atomix/runtime/atomic/value/v1"
	"github.com/atomix/runtime/sdk/pkg/errors"
	"github.com/atomix/runtime/sdk/pkg/logging"
	"github.com/atomix/runtime/sdk/pkg/runtime"
	"google.golang.org/grpc"
	"io"
)

var log = logging.GetLogger()

const Service = "atomix.multiraft.atomic.value.v1.AtomicValue"

func NewAtomicValueServer(protocol *client.Protocol, config api.AtomicValueConfig) valuev1.AtomicValueServer {
	return &AtomicValueServer{
		Protocol: protocol,
	}
}

type AtomicValueServer struct {
	*client.Protocol
}

func (s *AtomicValueServer) Create(ctx context.Context, request *valuev1.CreateRequest) (*valuev1.CreateResponse, error) {
	log.Debugw("Create",
		logging.Stringer("CreateRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(ctx)
	if err != nil {
		log.Warnw("Create",
			logging.Stringer("CreateRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	spec := multiraftv1.PrimitiveSpec{
		Service:   Service,
		Namespace: runtime.GetNamespace(),
		Name:      request.ID.Name,
	}
	if err := session.CreatePrimitive(ctx, spec); err != nil {
		log.Warnw("Create",
			logging.Stringer("CreateRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	response := &valuev1.CreateResponse{}
	log.Debugw("Create",
		logging.Stringer("CreateRequest", request),
		logging.Stringer("CreateResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Close(ctx context.Context, request *valuev1.CloseRequest) (*valuev1.CloseResponse, error) {
	log.Debugw("Close",
		logging.Stringer("CloseRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(ctx)
	if err != nil {
		log.Warnw("Close",
			logging.Stringer("CloseRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	if err := session.ClosePrimitive(ctx, request.ID.Name); err != nil {
		log.Warnw("Close",
			logging.Stringer("CloseRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	response := &valuev1.CloseResponse{}
	log.Debugw("Close",
		logging.Stringer("CloseRequest", request),
		logging.Stringer("CloseResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Set(ctx context.Context, request *valuev1.SetRequest) (*valuev1.SetResponse, error) {
	log.Debugw("Set",
		logging.Stringer("SetRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(ctx)
	if err != nil {
		log.Warnw("Set",
			logging.Stringer("SetRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	primitive, err := session.GetPrimitive(request.ID.Name)
	if err != nil {
		log.Warnw("Set",
			logging.Stringer("SetRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	command := client.Command[*api.SetResponse](primitive)
	output, err := command.Run(func(conn *grpc.ClientConn, headers *multiraftv1.CommandRequestHeaders) (*api.SetResponse, error) {
		return api.NewAtomicValueClient(conn).Set(ctx, &api.SetRequest{
			Headers: headers,
			SetInput: &api.SetInput{
				Value: request.Value,
			},
		})
	})
	if err != nil {
		log.Warnw("Set",
			logging.Stringer("SetRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	response := &valuev1.SetResponse{
		Version: uint64(output.Index),
	}
	log.Debugw("Set",
		logging.Stringer("SetRequest", request),
		logging.Stringer("SetResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Get(ctx context.Context, request *valuev1.GetRequest) (*valuev1.GetResponse, error) {
	log.Debugw("Get",
		logging.Stringer("GetRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(ctx)
	if err != nil {
		log.Warnw("Get",
			logging.Stringer("GetRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	primitive, err := session.GetPrimitive(request.ID.Name)
	if err != nil {
		log.Warnw("Get",
			logging.Stringer("GetRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	command := client.Query[*api.GetResponse](primitive)
	output, err := command.Run(func(conn *grpc.ClientConn, headers *multiraftv1.QueryRequestHeaders) (*api.GetResponse, error) {
		return api.NewAtomicValueClient(conn).Get(ctx, &api.GetRequest{
			Headers:  headers,
			GetInput: &api.GetInput{},
		})
	})
	if err != nil {
		log.Warnw("Get",
			logging.Stringer("GetRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	response := &valuev1.GetResponse{
		Value: &valuev1.Value{
			Value:   output.Value.Value,
			Version: uint64(output.Value.Index),
		},
	}
	log.Debugw("Get",
		logging.Stringer("GetRequest", request),
		logging.Stringer("GetResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Update(ctx context.Context, request *valuev1.UpdateRequest) (*valuev1.UpdateResponse, error) {
	log.Debugw("Update",
		logging.Stringer("UpdateRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(ctx)
	if err != nil {
		log.Warnw("Update",
			logging.Stringer("UpdateRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	primitive, err := session.GetPrimitive(request.ID.Name)
	if err != nil {
		log.Warnw("Update",
			logging.Stringer("UpdateRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	command := client.Command[*api.UpdateResponse](primitive)
	output, err := command.Run(func(conn *grpc.ClientConn, headers *multiraftv1.CommandRequestHeaders) (*api.UpdateResponse, error) {
		return api.NewAtomicValueClient(conn).Update(ctx, &api.UpdateRequest{
			Headers: headers,
			UpdateInput: &api.UpdateInput{
				Value:     request.Value,
				PrevIndex: multiraftv1.Index(request.PrevVersion),
				TTL:       request.TTL,
			},
		})
	})
	if err != nil {
		log.Warnw("Update",
			logging.Stringer("UpdateRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	response := &valuev1.UpdateResponse{
		Version: uint64(output.Index),
		PrevValue: &valuev1.Value{
			Value:   output.PrevValue.Value,
			Version: uint64(output.PrevValue.Index),
		},
	}
	log.Debugw("Update",
		logging.Stringer("UpdateRequest", request),
		logging.Stringer("UpdateResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Delete(ctx context.Context, request *valuev1.DeleteRequest) (*valuev1.DeleteResponse, error) {
	log.Debugw("Delete",
		logging.Stringer("DeleteRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(ctx)
	if err != nil {
		log.Warnw("Delete",
			logging.Stringer("DeleteRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	primitive, err := session.GetPrimitive(request.ID.Name)
	if err != nil {
		log.Warnw("Delete",
			logging.Stringer("DeleteRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	command := client.Command[*api.DeleteResponse](primitive)
	output, err := command.Run(func(conn *grpc.ClientConn, headers *multiraftv1.CommandRequestHeaders) (*api.DeleteResponse, error) {
		return api.NewAtomicValueClient(conn).Delete(ctx, &api.DeleteRequest{
			Headers: headers,
			DeleteInput: &api.DeleteInput{
				PrevIndex: multiraftv1.Index(request.PrevVersion),
			},
		})
	})
	if err != nil {
		log.Warnw("Delete",
			logging.Stringer("DeleteRequest", request),
			logging.Error("Error", err))
		return nil, errors.ToProto(err)
	}
	response := &valuev1.DeleteResponse{
		Value: &valuev1.Value{
			Value:   output.Value.Value,
			Version: uint64(output.Value.Index),
		},
	}
	log.Debugw("Delete",
		logging.Stringer("DeleteRequest", request),
		logging.Stringer("DeleteResponse", response))
	return response, nil
}

func (s *AtomicValueServer) Events(request *valuev1.EventsRequest, server valuev1.AtomicValue_EventsServer) error {
	log.Debugw("Events",
		logging.Stringer("EventsRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(server.Context())
	if err != nil {
		log.Warnw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Error("Error", err))
		return errors.ToProto(err)
	}
	primitive, err := session.GetPrimitive(request.ID.Name)
	if err != nil {
		log.Warnw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Error("Error", err))
		return errors.ToProto(err)
	}
	command := client.StreamCommand[api.AtomicValue_EventsClient, *api.EventsResponse](primitive)
	stream, err := command.Open(func(conn *grpc.ClientConn, headers *multiraftv1.CommandRequestHeaders) (api.AtomicValue_EventsClient, error) {
		return api.NewAtomicValueClient(conn).Events(server.Context(), &api.EventsRequest{
			Headers:     headers,
			EventsInput: &api.EventsInput{},
		})
	})
	if err != nil {
		err = errors.ToProto(err)
		log.Warnw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Error("Error", err))
		return err
	}
	for {
		output, err := command.Recv(stream.Recv)
		if err == io.EOF {
			log.Debugw("Events",
				logging.Stringer("EventsRequest", request),
				logging.String("State", "Done"))
			return nil
		}
		if err != nil {
			log.Warnw("Events",
				logging.Stringer("EventsRequest", request),
				logging.Error("Error", err))
			return errors.ToProto(err)
		}
		var response *valuev1.EventsResponse
		switch e := output.Event.Event.(type) {
		case *api.Event_Created_:
			response = &valuev1.EventsResponse{
				Event: valuev1.Event{
					Event: &valuev1.Event_Created_{
						Created: &valuev1.Event_Created{
							Value: valuev1.Value{
								Value:   e.Created.Value.Value,
								Version: uint64(e.Created.Value.Index),
							},
						},
					},
				},
			}
		case *api.Event_Updated_:
			response = &valuev1.EventsResponse{
				Event: valuev1.Event{
					Event: &valuev1.Event_Updated_{
						Updated: &valuev1.Event_Updated{
							Value: valuev1.Value{
								Value:   e.Updated.Value.Value,
								Version: uint64(e.Updated.Value.Index),
							},
							PrevValue: valuev1.Value{
								Value:   e.Updated.PrevValue.Value,
								Version: uint64(e.Updated.PrevValue.Index),
							},
						},
					},
				},
			}
		case *api.Event_Deleted_:
			response = &valuev1.EventsResponse{
				Event: valuev1.Event{
					Event: &valuev1.Event_Deleted_{
						Deleted: &valuev1.Event_Deleted{
							Value: valuev1.Value{
								Value:   e.Deleted.Value.Value,
								Version: uint64(e.Deleted.Value.Index),
							},
							Expired: e.Deleted.Expired,
						},
					},
				},
			}
		}
		log.Debugw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Stringer("EventsResponse", response))
		if err := server.Send(response); err != nil {
			log.Warnw("Events",
				logging.Stringer("EventsRequest", request),
				logging.Stringer("EventsResponse", response),
				logging.Error("Error", err))
			return err
		}
	}
}

func (s *AtomicValueServer) Watch(request *valuev1.WatchRequest, server valuev1.AtomicValue_WatchServer) error {
	log.Debugw("Events",
		logging.Stringer("EventsRequest", request))
	partition := s.PartitionBy([]byte(request.ID.Name))
	session, err := partition.GetSession(server.Context())
	if err != nil {
		log.Warnw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Error("Error", err))
		return errors.ToProto(err)
	}
	primitive, err := session.GetPrimitive(request.ID.Name)
	if err != nil {
		log.Warnw("Events",
			logging.Stringer("EventsRequest", request),
			logging.Error("Error", err))
		return errors.ToProto(err)
	}
	query := client.StreamQuery[api.AtomicValue_WatchClient, *api.WatchResponse](primitive)
	stream, err := query.Open(func(conn *grpc.ClientConn, headers *multiraftv1.QueryRequestHeaders) (api.AtomicValue_WatchClient, error) {
		return api.NewAtomicValueClient(conn).Watch(server.Context(), &api.WatchRequest{
			Headers:    headers,
			WatchInput: &api.WatchInput{},
		})
	})
	if err != nil {
		log.Warnw("Watch",
			logging.Stringer("WatchRequest", request),
			logging.Error("Error", err))
		return errors.ToProto(err)
	}
	for {
		output, err := query.Recv(stream.Recv)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Warnw("Watch",
				logging.Stringer("WatchRequest", request),
				logging.Error("Error", err))
			return errors.ToProto(err)
		}
		response := &valuev1.WatchResponse{
			Value: &valuev1.Value{
				Value:   output.Value.Value,
				Version: uint64(output.Value.Index),
			},
		}
		log.Debugw("Watch",
			logging.Stringer("WatchRequest", request),
			logging.Stringer("WatchResponse", response))
		if err := server.Send(response); err != nil {
			log.Warnw("Watch",
				logging.Stringer("WatchRequest", request),
				logging.Stringer("WatchResponse", response),
				logging.Error("Error", err))
			return err
		}
	}
}

var _ valuev1.AtomicValueServer = (*AtomicValueServer)(nil)