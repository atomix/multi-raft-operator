// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	multiraftv1 "github.com/atomix/multi-raft-storage/api/atomix/multiraft/v1"
	"github.com/atomix/multi-raft-storage/node/pkg/statemachine"
	"github.com/atomix/multi-raft-storage/node/pkg/statemachine/snapshot"
	"github.com/atomix/runtime/sdk/pkg/errors"
	"github.com/atomix/runtime/sdk/pkg/logging"
	"github.com/bits-and-blooms/bloom/v3"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"testing"
	"time"
)

func newManagerTestContext(ctrl *gomock.Controller) *managerTestContext {
	context := statemachine.NewMockSessionManagerContext(ctrl)
	context.EXPECT().Time().Return(time.UnixMilli(0)).AnyTimes()
	context.EXPECT().Log().Return(logging.GetLogger()).AnyTimes()
	return &managerTestContext{
		MockSessionManagerContext: context,
		sequenceNums:              make(map[ID]multiraftv1.SequenceNum),
	}
}

type managerTestContext struct {
	*statemachine.MockSessionManagerContext
	index        statemachine.Index
	sequenceNums map[ID]multiraftv1.SequenceNum
	queryID      statemachine.QueryID
}

func (c *managerTestContext) nextProposalID() statemachine.ProposalID {
	c.index++
	c.EXPECT().Index().Return(c.index).AnyTimes()
	return statemachine.ProposalID(c.index)
}

func (c *managerTestContext) nextSequenceNum(sessionID ID) multiraftv1.SequenceNum {
	sequenceNum := c.sequenceNums[sessionID] + 1
	c.sequenceNums[sessionID] = sequenceNum
	return sequenceNum
}

func (c *managerTestContext) lastSequenceNum() multiraftv1.SequenceNum {
	return multiraftv1.SequenceNum(c.index)
}

func (c *managerTestContext) nextQueryID() statemachine.QueryID {
	c.queryID++
	return c.queryID
}

func TestOpenCloseSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	context := newManagerTestContext(ctrl)
	timer := statemachine.NewMockTimer(ctrl)
	scheduler := statemachine.NewMockScheduler(ctrl)
	scheduler.EXPECT().Schedule(gomock.Any(), gomock.Any()).Return(timer).AnyTimes()
	context.EXPECT().Scheduler().Return(scheduler).AnyTimes()

	primitives := NewMockPrimitiveManager(ctrl)
	manager := NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})

	// Open a new session
	proposalID := context.nextProposalID()
	sessionID := ID(proposalID)
	openSession := statemachine.NewMockOpenSessionProposal(ctrl)
	openSession.EXPECT().ID().Return(proposalID).AnyTimes()
	openSession.EXPECT().Input().Return(&multiraftv1.OpenSessionInput{
		Timeout: time.Minute,
	}).AnyTimes()
	openSession.EXPECT().Close()
	openSession.EXPECT().Output(gomock.Any())
	manager.OpenSession(openSession)

	// Verify the session is in the context
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	session, ok := manager.(Context).Sessions().Get(sessionID)
	assert.True(t, ok)
	assert.Equal(t, sessionID, session.ID())

	// Take a snapshot of the manager and create a new manager from the snapshot
	buf := &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))

	// Verify the session is in the context after recovering from a snapshot
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	session, ok = manager.(Context).Sessions().Get(sessionID)
	assert.True(t, ok)
	assert.Equal(t, sessionID, session.ID())

	// Close the session and verify it is removed from the session manager context
	timer.EXPECT().Cancel()
	closeSession := statemachine.NewMockCloseSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	closeSession.EXPECT().ID().Return(proposalID).AnyTimes()
	closeSession.EXPECT().Input().Return(&multiraftv1.CloseSessionInput{
		SessionID: multiraftv1.SessionID(sessionID),
	}).AnyTimes()
	closeSession.EXPECT().Output(gomock.Any())
	closeSession.EXPECT().Close()
	manager.CloseSession(closeSession)

	// Verify the session has been removed from the snapshot
	assert.Len(t, manager.(Context).Sessions().List(), 0)
	session, ok = manager.(Context).Sessions().Get(sessionID)
	assert.False(t, ok)
	assert.Nil(t, session)
}

func TestExpireSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	context := newManagerTestContext(ctrl)
	timer := statemachine.NewMockTimer(ctrl)
	scheduler := statemachine.NewMockScheduler(ctrl)
	scheduler.EXPECT().Time().Return(time.UnixMilli(0)).AnyTimes()
	var expireFunc func()
	scheduler.EXPECT().Schedule(gomock.Any(), gomock.Any()).DoAndReturn(func(expire time.Time, f func()) statemachine.Timer {
		assert.Equal(t, time.UnixMilli(0).Add(time.Minute), expire)
		expireFunc = f
		return timer
	})
	context.EXPECT().Scheduler().Return(scheduler).AnyTimes()

	primitives := NewMockPrimitiveManager(ctrl)
	manager := NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})

	// Open a new session
	proposalID := context.nextProposalID()
	sessionID := ID(proposalID)
	openSession := statemachine.NewMockOpenSessionProposal(ctrl)
	openSession.EXPECT().ID().Return(proposalID).AnyTimes()
	openSession.EXPECT().Input().Return(&multiraftv1.OpenSessionInput{
		Timeout: time.Minute,
	}).AnyTimes()
	openSession.EXPECT().Close()
	openSession.EXPECT().Output(gomock.Any())
	manager.OpenSession(openSession)

	// Verify the session is in the context
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	session, ok := manager.(Context).Sessions().Get(sessionID)
	assert.True(t, ok)
	assert.Equal(t, sessionID, session.ID())

	closed := false
	session.Watch(func(state State) {
		assert.Equal(t, Closed, state)
		closed = true
	})

	// Call the expiration function
	context.EXPECT().Time().Return(time.UnixMilli(0).Add(time.Minute)).AnyTimes()
	scheduler.EXPECT().Time().Return(time.UnixMilli(0).Add(time.Minute)).AnyTimes()
	expireFunc()

	// Verify the session was closed and removed from the manager
	assert.True(t, closed)
	assert.Len(t, manager.(Context).Sessions().List(), 0)
	session, ok = manager.(Context).Sessions().Get(sessionID)
	assert.False(t, ok)
	assert.Nil(t, session)
}

func TestCreateClosePrimitive(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	context := newManagerTestContext(ctrl)
	timer := statemachine.NewMockTimer(ctrl)
	scheduler := statemachine.NewMockScheduler(ctrl)
	scheduler.EXPECT().Time().Return(time.UnixMilli(0)).AnyTimes()
	scheduler.EXPECT().Schedule(gomock.Any(), gomock.Any()).Return(timer).AnyTimes()
	context.EXPECT().Scheduler().Return(scheduler).AnyTimes()

	primitives := NewMockPrimitiveManager(ctrl)
	manager := NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})

	// Open a new session
	proposalID := context.nextProposalID()
	sessionID := ID(proposalID)
	openSession := statemachine.NewMockOpenSessionProposal(ctrl)
	openSession.EXPECT().ID().Return(proposalID).AnyTimes()
	openSession.EXPECT().Input().Return(&multiraftv1.OpenSessionInput{
		Timeout: time.Minute,
	}).AnyTimes()
	openSession.EXPECT().Close()
	openSession.EXPECT().Output(gomock.Any())
	manager.OpenSession(openSession)
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Take a snapshot of the manager and create a new manager from the snapshot
	buf := &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Create a primitive using the session
	proposal := statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	sequenceNum := context.nextSequenceNum(sessionID)
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_CreatePrimitive{
			CreatePrimitive: &multiraftv1.CreatePrimitiveInput{
				PrimitiveSpec: multiraftv1.PrimitiveSpec{
					Service:   "test",
					Namespace: "foo",
					Name:      "bar",
				},
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	primitives.EXPECT().CreatePrimitive(gomock.Any()).Do(func(proposal CreatePrimitiveProposal) {
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 0)
		assert.Len(t, manager.(Context).Proposals().List(), 0)
		proposal.Output(&multiraftv1.CreatePrimitiveOutput{
			PrimitiveID: 1,
		})
		proposal.Close()
	})
	manager.Propose(proposal)

	// Retry the create primitive command and verify the primitive manager is not called again
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_CreatePrimitive{
			CreatePrimitive: &multiraftv1.CreatePrimitiveInput{
				PrimitiveSpec: multiraftv1.PrimitiveSpec{
					Service:   "test",
					Namespace: "foo",
					Name:      "bar",
				},
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	manager.Propose(proposal)

	// Take a snapshot of the manager and retry creating the primitive one more time
	buf = &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Retry the create primitive command and verify the primitive manager is not called again
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_CreatePrimitive{
			CreatePrimitive: &multiraftv1.CreatePrimitiveInput{
				PrimitiveSpec: multiraftv1.PrimitiveSpec{
					Service:   "test",
					Namespace: "foo",
					Name:      "bar",
				},
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	manager.Propose(proposal)

	// Close the primitive
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	sequenceNum = context.nextSequenceNum(sessionID)
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_ClosePrimitive{
			ClosePrimitive: &multiraftv1.ClosePrimitiveInput{
				PrimitiveID: 1,
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	primitives.EXPECT().ClosePrimitive(gomock.Any()).Do(func(proposal ClosePrimitiveProposal) {
		assert.Equal(t, sessionID, proposal.Session().ID())
		proposal.Output(&multiraftv1.ClosePrimitiveOutput{})
		proposal.Close()
	})
	manager.Propose(proposal)
}

func TestUnaryProposal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	context := newManagerTestContext(ctrl)
	timer := statemachine.NewMockTimer(ctrl)
	timer.EXPECT().Cancel().AnyTimes()
	scheduler := statemachine.NewMockScheduler(ctrl)
	scheduler.EXPECT().Time().Return(time.UnixMilli(0)).AnyTimes()
	scheduler.EXPECT().Schedule(gomock.Any(), gomock.Any()).Return(timer).AnyTimes()
	context.EXPECT().Scheduler().Return(scheduler).AnyTimes()

	primitives := NewMockPrimitiveManager(ctrl)
	manager := NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})

	// Submit a proposal for an unknown session and verify the returned error
	proposal := statemachine.NewMockSessionProposal(ctrl)
	proposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   1,
		SequenceNum: context.nextSequenceNum(1),
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Error(gomock.Any()).Do(func(err error) {
		assert.True(t, errors.IsForbidden(err))
	})
	proposal.EXPECT().Close()
	manager.Propose(proposal)

	// Open a new session
	proposalID := context.nextProposalID()
	sessionID := ID(proposalID)
	openSession := statemachine.NewMockOpenSessionProposal(ctrl)
	openSession.EXPECT().ID().Return(proposalID).AnyTimes()
	openSession.EXPECT().Input().Return(&multiraftv1.OpenSessionInput{
		Timeout: time.Minute,
	}).AnyTimes()
	openSession.EXPECT().Output(gomock.Any())
	openSession.EXPECT().Close()
	manager.OpenSession(openSession)
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Submit a primitive proposal and verify the proposal is applied to the primitive
	proposal1 := statemachine.NewMockSessionProposal(ctrl)
	proposalID1 := context.nextProposalID()
	sequenceNum1 := context.nextSequenceNum(sessionID)
	proposal1.EXPECT().ID().Return(proposalID1).AnyTimes()
	proposal1.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum1,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal1.EXPECT().Output(gomock.Any())
	proposal1.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		assert.Equal(t, proposalID1, proposal.ID())
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 1)
		p, ok := proposal.Session().Proposals().Get(proposalID1)
		assert.True(t, ok)
		assert.Equal(t, proposalID1, p.ID())
		assert.Len(t, manager.(Context).Proposals().List(), 1)
		p, ok = manager.(Context).Proposals().Get(proposalID1)
		assert.True(t, ok)
		assert.Equal(t, proposalID1, p.ID())
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("bar"),
		})
		proposal.Close()
		assert.Len(t, proposal.Session().Proposals().List(), 0)
		assert.Len(t, manager.(Context).Proposals().List(), 0)
	})
	manager.Propose(proposal1)

	// Retry the same primitive proposal and verify the proposal is not applied to the primitive again (for linearizability)
	proposal1 = statemachine.NewMockSessionProposal(ctrl)
	proposal1.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal1.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum1,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal1.EXPECT().Output(gomock.Any())
	proposal1.EXPECT().Close()
	manager.Propose(proposal1)

	// Take a snapshot of the manager and create a new manager from the snapshot
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)
	buf := &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)

	// Retry the same primitive proposal again after the snapshot
	proposal1 = statemachine.NewMockSessionProposal(ctrl)
	proposal1.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal1.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum1,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal1.EXPECT().Output(gomock.Any())
	proposal1.EXPECT().Close()
	manager.Propose(proposal1)

	// Submit a primitive proposal and verify the proposal is applied to the primitive
	proposal2 := statemachine.NewMockSessionProposal(ctrl)
	proposalID2 := context.nextProposalID()
	sequenceNum2 := context.nextSequenceNum(sessionID)
	proposal2.EXPECT().ID().Return(proposalID2).AnyTimes()
	proposal2.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum2,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal2.EXPECT().Output(gomock.Any())
	proposal2.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		assert.Equal(t, proposalID2, proposal.ID())
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 1)
		p, ok := proposal.Session().Proposals().Get(proposalID2)
		assert.True(t, ok)
		assert.Equal(t, proposalID2, p.ID())
		assert.Len(t, manager.(Context).Proposals().List(), 1)
		p, ok = manager.(Context).Proposals().Get(proposalID2)
		assert.True(t, ok)
		assert.Equal(t, proposalID2, p.ID())
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("baz"),
		})
		proposal.Close()
		assert.Len(t, proposal.Session().Proposals().List(), 0)
		assert.Len(t, manager.(Context).Proposals().List(), 0)
	})
	manager.Propose(proposal2)

	// Send a keep-alive to ack one proposal
	inputFilter := bloom.NewWithEstimates(2, .05)
	sequenceNum2Bytes := make([]byte, 8)
	binary.BigEndian.PutUint64(sequenceNum2Bytes, uint64(sequenceNum2))
	inputFilter.Add(sequenceNum2Bytes)
	inputFilterBytes, err := json.Marshal(inputFilter)
	assert.NoError(t, err)
	keepAlive := statemachine.NewMockKeepAliveProposal(ctrl)
	keepAlive.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	keepAlive.EXPECT().Input().Return(&multiraftv1.KeepAliveInput{
		SessionID:            multiraftv1.SessionID(sessionID),
		InputFilter:          inputFilterBytes,
		LastInputSequenceNum: sequenceNum2,
	}).AnyTimes()
	keepAlive.EXPECT().Output(gomock.Any())
	keepAlive.EXPECT().Close()
	manager.KeepAlive(keepAlive)

	// Verify the first proposal is unknown to the session
	proposal1 = statemachine.NewMockSessionProposal(ctrl)
	proposal1.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal1.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum1,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal1.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		proposal.Close()
	})
	manager.Propose(proposal1)

	// Retry the second proposal and verify it is still responding
	proposal2 = statemachine.NewMockSessionProposal(ctrl)
	proposal2.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal2.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum2,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	proposal2.EXPECT().Output(gomock.Any())
	proposal2.EXPECT().Close()
	manager.Propose(proposal2)
}

func TestStreamingProposal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	context := newManagerTestContext(ctrl)
	timer := statemachine.NewMockTimer(ctrl)
	timer.EXPECT().Cancel().AnyTimes()
	scheduler := statemachine.NewMockScheduler(ctrl)
	scheduler.EXPECT().Time().Return(time.UnixMilli(0)).AnyTimes()
	scheduler.EXPECT().Schedule(gomock.Any(), gomock.Any()).Return(timer).AnyTimes()
	context.EXPECT().Scheduler().Return(scheduler).AnyTimes()

	primitives := NewMockPrimitiveManager(ctrl)
	manager := NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})

	// Open a new session
	proposalID := context.nextProposalID()
	sessionID := ID(proposalID)
	openSession := statemachine.NewMockOpenSessionProposal(ctrl)
	openSession.EXPECT().ID().Return(proposalID).AnyTimes()
	openSession.EXPECT().Input().Return(&multiraftv1.OpenSessionInput{
		Timeout: time.Minute,
	}).AnyTimes()
	openSession.EXPECT().Output(gomock.Any())
	openSession.EXPECT().Close()
	manager.OpenSession(openSession)
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Submit a primitive proposal and verify the proposal is applied to the primitive
	streamProposal := statemachine.NewMockSessionProposal(ctrl)
	streamProposalID := context.nextProposalID()
	streamSequenceNum := context.nextSequenceNum(sessionID)
	streamProposal.EXPECT().ID().Return(streamProposalID).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(1), output.SequenceNum)
		assert.Equal(t, "a", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		assert.Equal(t, streamProposalID, proposal.ID())
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 1)
		p, ok := proposal.Session().Proposals().Get(streamProposalID)
		assert.True(t, ok)
		assert.Equal(t, streamProposalID, p.ID())
		assert.Len(t, manager.(Context).Proposals().List(), 1)
		p, ok = manager.(Context).Proposals().Get(streamProposalID)
		assert.True(t, ok)
		assert.Equal(t, streamProposalID, p.ID())
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("a"),
		})
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("b"),
		})
	})
	manager.Propose(streamProposal)

	// Retry the same primitive proposal and verify the proposal is not applied to the primitive again (for linearizability)
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(1), output.SequenceNum)
		assert.Equal(t, "a", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	manager.Propose(streamProposal)

	// Take a snapshot of the manager and create a new manager from the snapshot
	assert.Len(t, manager.(Context).Proposals().List(), 1)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 1)
	buf := &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 1)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 1)

	// Retry the same primitive proposal again after the snapshot
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(1), output.SequenceNum)
		assert.Equal(t, "a", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	manager.Propose(streamProposal)

	// Submit a separate proposal producing more outputs to the streaming proposal
	proposal := statemachine.NewMockSessionProposal(ctrl)
	proposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: context.nextSequenceNum(sessionID),
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("bar"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(3), output.SequenceNum)
		assert.Equal(t, "c", string(output.GetProposal().Payload))
	})
	proposal.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		streamProposal, ok := proposal.Session().Proposals().Get(streamProposalID)
		assert.True(t, ok)
		streamProposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("c"),
		})
		proposal.Close()
	})
	manager.Propose(proposal)

	// Retry the streaming proposal again to verify all 3 outputs are replayed
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(1), output.SequenceNum)
		assert.Equal(t, "a", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(3), output.SequenceNum)
		assert.Equal(t, "c", string(output.GetProposal().Payload))
	})
	manager.Propose(streamProposal)

	// Send a keep-alive to ack one of the streaming proposal outputs
	inputFilter := bloom.NewWithEstimates(2, .05)
	streamSequenceNumBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(streamSequenceNumBytes, uint64(streamSequenceNum))
	inputFilter.Add(streamSequenceNumBytes)
	inputFilterBytes, err := json.Marshal(inputFilter)
	assert.NoError(t, err)
	keepAlive := statemachine.NewMockKeepAliveProposal(ctrl)
	keepAlive.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	keepAlive.EXPECT().Input().Return(&multiraftv1.KeepAliveInput{
		SessionID:            multiraftv1.SessionID(sessionID),
		InputFilter:          inputFilterBytes,
		LastInputSequenceNum: context.lastSequenceNum(),
		LastOutputSequenceNums: map[multiraftv1.SequenceNum]multiraftv1.SequenceNum{
			streamSequenceNum: 1,
		},
	}).AnyTimes()
	keepAlive.EXPECT().Output(gomock.Any())
	keepAlive.EXPECT().Close()
	manager.KeepAlive(keepAlive)

	// Retry the streaming proposal and verify only the two unacknowledged outputs are replayed
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(3), output.SequenceNum)
		assert.Equal(t, "c", string(output.GetProposal().Payload))
	})
	manager.Propose(streamProposal)

	// Submit a proposal to close the streaming proposal
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: context.nextSequenceNum(sessionID),
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("bar"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Close()
	proposal.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		streamProposal, ok := proposal.Session().Proposals().Get(streamProposalID)
		assert.True(t, ok)
		streamProposal.Close()
		proposal.Close()
	})
	manager.Propose(proposal)

	// Retry the streaming proposal and verify only the two unacknowledged outputs are replayed and the proposal is closed
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(3), output.SequenceNum)
		assert.Equal(t, "c", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Close()
	manager.Propose(streamProposal)

	// Take a snapshot of the manager and create a new manager from the snapshot
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)
	buf = &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)

	// Retry the streaming proposal to verify outputs again
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(2), output.SequenceNum)
		assert.Equal(t, "b", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Output(gomock.Any()).Do(func(output *multiraftv1.SessionProposalOutput) {
		assert.Equal(t, multiraftv1.SequenceNum(3), output.SequenceNum)
		assert.Equal(t, "c", string(output.GetProposal().Payload))
	})
	streamProposal.EXPECT().Close()
	manager.Propose(streamProposal)

	// Send another keep-alive acking the remaining outputs
	inputFilter = bloom.NewWithEstimates(2, .05)
	streamSequenceNumBytes = make([]byte, 8)
	binary.BigEndian.PutUint64(streamSequenceNumBytes, uint64(streamSequenceNum))
	inputFilter.Add(streamSequenceNumBytes)
	inputFilterBytes, err = json.Marshal(inputFilter)
	assert.NoError(t, err)
	keepAlive = statemachine.NewMockKeepAliveProposal(ctrl)
	keepAlive.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	keepAlive.EXPECT().Input().Return(&multiraftv1.KeepAliveInput{
		SessionID:            multiraftv1.SessionID(sessionID),
		InputFilter:          inputFilterBytes,
		LastInputSequenceNum: context.lastSequenceNum(),
		LastOutputSequenceNums: map[multiraftv1.SequenceNum]multiraftv1.SequenceNum{
			streamSequenceNum: 3,
		},
	}).AnyTimes()
	keepAlive.EXPECT().Output(gomock.Any())
	keepAlive.EXPECT().Close()
	manager.KeepAlive(keepAlive)

	// Retry the streaming proposal to verify the close is replayed
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Close()
	manager.Propose(streamProposal)

	// Take a snapshot of the manager and create a new manager from the snapshot
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)
	buf = &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)

	// Retry the streaming proposal to verify the close is replayed
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: streamSequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("foo"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Close()
	manager.Propose(streamProposal)

	// Send a keep-alive to ack the streaming proposal
	inputFilter = bloom.NewWithEstimates(2, .05)
	inputFilterBytes, err = json.Marshal(inputFilter)
	assert.NoError(t, err)
	keepAlive = statemachine.NewMockKeepAliveProposal(ctrl)
	keepAlive.EXPECT().ID().Return(context.nextProposalID()).AnyTimes()
	keepAlive.EXPECT().Input().Return(&multiraftv1.KeepAliveInput{
		SessionID:            multiraftv1.SessionID(sessionID),
		InputFilter:          inputFilterBytes,
		LastInputSequenceNum: context.lastSequenceNum(),
		LastOutputSequenceNums: map[multiraftv1.SequenceNum]multiraftv1.SequenceNum{
			streamSequenceNum: 3,
		},
	}).AnyTimes()
	keepAlive.EXPECT().Output(gomock.Any())
	keepAlive.EXPECT().Close()
	manager.KeepAlive(keepAlive)
}

func TestQuery(t *testing.T) {

}

func TestStreamingQuery(t *testing.T) {

}

func TestManager(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	context := newManagerTestContext(ctrl)
	timer := statemachine.NewMockTimer(ctrl)
	scheduler := statemachine.NewMockScheduler(ctrl)
	scheduler.EXPECT().Time().Return(time.UnixMilli(0)).AnyTimes()
	scheduler.EXPECT().Schedule(gomock.Any(), gomock.Any()).Return(timer).AnyTimes()
	context.EXPECT().Scheduler().Return(scheduler).AnyTimes()

	primitives := NewMockPrimitiveManager(ctrl)
	manager := NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})

	// Open a new session
	proposalID := context.nextProposalID()
	sessionID := ID(proposalID)
	openSession := statemachine.NewMockOpenSessionProposal(ctrl)
	openSession.EXPECT().ID().Return(proposalID).AnyTimes()
	openSession.EXPECT().Input().Return(&multiraftv1.OpenSessionInput{
		Timeout: time.Minute,
	}).AnyTimes()
	openSession.EXPECT().Close()
	openSession.EXPECT().Output(gomock.Any())
	manager.OpenSession(openSession)
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Take a snapshot of the manager and create a new manager from the snapshot
	buf := &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())

	// Create a primitive using the session
	proposal := statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	sequenceNum := context.nextSequenceNum(sessionID)
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_CreatePrimitive{
			CreatePrimitive: &multiraftv1.CreatePrimitiveInput{
				PrimitiveSpec: multiraftv1.PrimitiveSpec{
					Service:   "test",
					Namespace: "foo",
					Name:      "bar",
				},
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	primitives.EXPECT().CreatePrimitive(gomock.Any()).Do(func(proposal CreatePrimitiveProposal) {
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 0)
		assert.Len(t, manager.(Context).Proposals().List(), 0)
		proposal.Output(&multiraftv1.CreatePrimitiveOutput{
			PrimitiveID: 1,
		})
		proposal.Close()
	})
	manager.Propose(proposal)

	// Retry the create primitive command and verify the primitive manager is not called again
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_CreatePrimitive{
			CreatePrimitive: &multiraftv1.CreatePrimitiveInput{
				PrimitiveSpec: multiraftv1.PrimitiveSpec{
					Service:   "test",
					Namespace: "foo",
					Name:      "bar",
				},
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	manager.Propose(proposal)

	// Submit a primitive proposal and verify the proposal is applied to the primitive
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	sequenceNum = context.nextSequenceNum(sessionID)
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("Hello"),
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		assert.Equal(t, proposalID, proposal.ID())
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 1)
		p, ok := proposal.Session().Proposals().Get(proposalID)
		assert.True(t, ok)
		assert.Equal(t, proposalID, p.ID())
		assert.Len(t, manager.(Context).Proposals().List(), 1)
		p, ok = manager.(Context).Proposals().Get(proposalID)
		assert.True(t, ok)
		assert.Equal(t, proposalID, p.ID())
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("world!"),
		})
		proposal.Close()
		assert.Len(t, proposal.Session().Proposals().List(), 0)
		assert.Len(t, manager.(Context).Proposals().List(), 0)
	})
	manager.Propose(proposal)

	// Retry the same primitive proposal and verify the proposal is not applied to the primitive again (for linearizability)
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("Hello"),
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	manager.Propose(proposal)

	// Take another snapshot of the manager and create a new manager from the snapshot
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)
	buf = &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 0)
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 0)

	// Retry the same primitive proposal again after the snapshot
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("Hello"),
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	manager.Propose(proposal)

	// Submit a primitive query and verify the query is applied to the primitive
	query := statemachine.NewMockSessionQuery(ctrl)
	queryID := context.nextQueryID()
	query.EXPECT().ID().Return(queryID).AnyTimes()
	query.EXPECT().Input().Return(&multiraftv1.SessionQueryInput{
		SessionID: multiraftv1.SessionID(sessionID),
		Input: &multiraftv1.SessionQueryInput_Query{
			Query: &multiraftv1.PrimitiveQueryInput{
				PrimitiveID: 1,
			},
		},
	}).AnyTimes()
	query.EXPECT().Output(gomock.Any())
	query.EXPECT().Close()
	primitives.EXPECT().Query(gomock.Any()).Do(func(query PrimitiveQuery) {
		assert.Equal(t, queryID, query.ID())
		assert.Equal(t, sessionID, query.Session().ID())
		query.Output(&multiraftv1.PrimitiveQueryOutput{})
		query.Close()
	})
	manager.Query(query)

	// Submit a streaming (not closed) proposal
	streamProposal := statemachine.NewMockSessionProposal(ctrl)
	streamProposalID := context.nextProposalID()
	sequenceNum = context.nextSequenceNum(sessionID)
	streamProposal.EXPECT().ID().Return(streamProposalID).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("Hello"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Times(3)
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		assert.Equal(t, streamProposalID, proposal.ID())
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 1)
		p, ok := proposal.Session().Proposals().Get(streamProposalID)
		assert.True(t, ok)
		assert.Equal(t, streamProposalID, p.ID())
		assert.Len(t, manager.(Context).Proposals().List(), 1)
		p, ok = manager.(Context).Proposals().Get(streamProposalID)
		assert.True(t, ok)
		assert.Equal(t, streamProposalID, p.ID())
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("a"),
		})
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("b"),
		})
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("c"),
		})
	})
	manager.Propose(streamProposal)

	// Verify the proposal is still open in the manager context and session
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 1)
	p, ok := manager.(Context).Proposals().Get(streamProposalID)
	assert.True(t, ok)
	assert.Equal(t, streamProposalID, p.ID())
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 1)
	p, ok = manager.(Context).Sessions().List()[0].Proposals().Get(streamProposalID)
	assert.True(t, ok)
	assert.Equal(t, streamProposalID, p.ID())

	// Retry the streaming command and verify outputs are replayed
	streamProposal = statemachine.NewMockSessionProposal(ctrl)
	streamProposal.EXPECT().ID().Return(streamProposalID).AnyTimes()
	streamProposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("Hello"),
			},
		},
	}).AnyTimes()
	streamProposal.EXPECT().Output(gomock.Any()).Times(3)
	manager.Propose(streamProposal)

	// Verify the proposal is still open in the manager context and session
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 1)
	p, ok = manager.(Context).Proposals().Get(streamProposalID)
	assert.True(t, ok)
	assert.Equal(t, streamProposalID, p.ID())
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 1)
	p, ok = manager.(Context).Sessions().List()[0].Proposals().Get(streamProposalID)
	assert.True(t, ok)
	assert.Equal(t, streamProposalID, p.ID())

	// Verify sending additional outputs on the proposal stream
	streamProposal.EXPECT().Output(gomock.Any()).Times(2)
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	sequenceNum = context.nextSequenceNum(sessionID)
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_Proposal{
			Proposal: &multiraftv1.PrimitiveProposalInput{
				PrimitiveID: 1,
				Payload:     []byte("Hello"),
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	primitives.EXPECT().Propose(gomock.Any()).Do(func(proposal PrimitiveProposal) {
		streamProposal, ok := proposal.Session().Proposals().Get(streamProposalID)
		assert.True(t, ok)
		assert.Equal(t, streamProposalID, streamProposal.ID())
		streamProposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("e"),
		})
		streamProposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("f"),
		})
		proposal.Output(&multiraftv1.PrimitiveProposalOutput{
			Payload: []byte("world!"),
		})
		proposal.Close()
	})
	manager.Propose(proposal)

	// Take a snapshot of the manager and restore the manager state again
	buf = &bytes.Buffer{}
	primitives.EXPECT().Snapshot(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Snapshot(snapshot.NewWriter(buf)))
	manager = NewManager(context, func(Context) PrimitiveManager {
		return primitives
	})
	primitives.EXPECT().Recover(gomock.Any()).Return(nil)
	assert.NoError(t, manager.Recover(snapshot.NewReader(buf)))

	// Verify the streaming proposal is still open again
	assert.Len(t, manager.(Context).Sessions().List(), 1)
	assert.Equal(t, sessionID, manager.(Context).Sessions().List()[0].ID())
	assert.Len(t, manager.(Context).Proposals().List(), 1)
	p, ok = manager.(Context).Proposals().Get(streamProposalID)
	assert.True(t, ok)
	assert.Equal(t, streamProposalID, p.ID())
	assert.Len(t, manager.(Context).Sessions().List()[0].Proposals().List(), 1)
	p, ok = manager.(Context).Sessions().List()[0].Proposals().Get(streamProposalID)
	assert.True(t, ok)
	assert.Equal(t, streamProposalID, p.ID())

	// Close the primitive
	proposal = statemachine.NewMockSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	sequenceNum = context.nextSequenceNum(sessionID)
	proposal.EXPECT().ID().Return(proposalID).AnyTimes()
	proposal.EXPECT().Input().Return(&multiraftv1.SessionProposalInput{
		SessionID:   multiraftv1.SessionID(sessionID),
		SequenceNum: sequenceNum,
		Input: &multiraftv1.SessionProposalInput_ClosePrimitive{
			ClosePrimitive: &multiraftv1.ClosePrimitiveInput{
				PrimitiveID: 1,
			},
		},
	}).AnyTimes()
	proposal.EXPECT().Output(gomock.Any())
	proposal.EXPECT().Close()
	primitives.EXPECT().ClosePrimitive(gomock.Any()).Do(func(proposal ClosePrimitiveProposal) {
		assert.Equal(t, sessionID, proposal.Session().ID())
		assert.Len(t, proposal.Session().Proposals().List(), 1)
		assert.Len(t, manager.(Context).Proposals().List(), 1)
		proposal.Output(&multiraftv1.ClosePrimitiveOutput{})
		proposal.Close()
	})
	manager.Propose(proposal)

	// Close the session and verify it is removed from the session manager context
	closeSession := statemachine.NewMockCloseSessionProposal(ctrl)
	proposalID = context.nextProposalID()
	closeSession.EXPECT().ID().Return(proposalID).AnyTimes()
	closeSession.EXPECT().Input().Return(&multiraftv1.CloseSessionInput{
		SessionID: multiraftv1.SessionID(sessionID),
	}).AnyTimes()
	closeSession.EXPECT().Output(gomock.Any())
	closeSession.EXPECT().Close()
	manager.CloseSession(closeSession)
	assert.Len(t, manager.(Context).Sessions().List(), 0)
}
