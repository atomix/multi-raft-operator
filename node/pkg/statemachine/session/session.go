// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"container/list"
	"encoding/binary"
	"encoding/json"
	multiraftv1 "github.com/atomix/multi-raft-storage/api/atomix/multiraft/v1"
	"github.com/atomix/multi-raft-storage/node/pkg/statemachine"
	"github.com/atomix/multi-raft-storage/node/pkg/statemachine/snapshot"
	"github.com/atomix/runtime/sdk/pkg/errors"
	"github.com/atomix/runtime/sdk/pkg/logging"
	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
	"time"
)

func newManagedSession(manager *sessionManagerStateMachine) *managedSession {
	return &managedSession{
		manager:          manager,
		proposals:        newPrimitiveProposals(),
		sessionProposals: make(map[multiraftv1.SequenceNum]*sessionProposal),
		watchers:         make(map[uuid.UUID]statemachine.WatchFunc[State]),
	}
}

type managedSession struct {
	manager          *sessionManagerStateMachine
	proposals        *primitiveProposals
	sessionProposals map[multiraftv1.SequenceNum]*sessionProposal
	log              logging.Logger
	id               ID
	state            State
	watchers         map[uuid.UUID]statemachine.WatchFunc[State]
	timeout          time.Duration
	lastUpdated      time.Time
	expireTimer      statemachine.Timer
}

func (s *managedSession) Log() logging.Logger {
	return s.log
}

func (s *managedSession) ID() ID {
	return s.id
}

func (s *managedSession) State() State {
	return s.state
}

func (s *managedSession) Watch(f statemachine.WatchFunc[State]) statemachine.CancelFunc {
	id := uuid.New()
	s.watchers[id] = f
	return func() {
		delete(s.watchers, id)
	}
}

func (s *managedSession) Proposals() Proposals {
	return s.proposals
}

func (s *managedSession) propose(proposal statemachine.SessionProposal) {
	if p, ok := s.sessionProposals[proposal.Input().SequenceNum]; ok {
		p.replay(proposal)
	} else {
		p := newSessionProposal(s)
		s.sessionProposals[proposal.Input().SequenceNum] = p
		p.execute(proposal)
	}
}

func (s *managedSession) query(query statemachine.SessionQuery) {
	q := newSessionQuery(s)
	q.execute(query)
}

func (s *managedSession) Snapshot(writer *snapshot.Writer) error {
	s.Log().Debug("Persisting session to snapshot")

	var state multiraftv1.SessionSnapshot_State
	switch s.state {
	case Open:
		state = multiraftv1.SessionSnapshot_OPEN
	case Closed:
		state = multiraftv1.SessionSnapshot_CLOSED
	}

	snapshot := &multiraftv1.SessionSnapshot{
		SessionID:   multiraftv1.SessionID(s.id),
		State:       state,
		Timeout:     s.timeout,
		LastUpdated: s.lastUpdated,
	}
	if err := writer.WriteMessage(snapshot); err != nil {
		return err
	}

	if err := writer.WriteVarInt(len(s.sessionProposals)); err != nil {
		return err
	}
	for _, proposal := range s.sessionProposals {
		if err := proposal.snapshot(writer); err != nil {
			return err
		}
	}
	return nil
}

func (s *managedSession) Recover(reader *snapshot.Reader) error {
	snapshot := &multiraftv1.SessionSnapshot{}
	if err := reader.ReadMessage(snapshot); err != nil {
		return err
	}

	s.id = ID(snapshot.SessionID)
	s.timeout = snapshot.Timeout
	s.lastUpdated = snapshot.LastUpdated

	s.log = s.manager.Log().WithFields(logging.Uint64("Session", uint64(s.id)))
	s.Log().Debug("Recovering session from snapshot")

	n, err := reader.ReadVarInt()
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		proposal := newSessionProposal(s)
		if err := proposal.recover(reader); err != nil {
			return err
		}
		s.sessionProposals[proposal.input.SequenceNum] = proposal
	}

	switch snapshot.State {
	case multiraftv1.SessionSnapshot_OPEN:
		s.state = Open
	case multiraftv1.SessionSnapshot_CLOSED:
		s.state = Closed
	}
	s.manager.sessions.add(s)
	s.scheduleExpireTimer()
	return nil
}

func (s *managedSession) expire() {
	s.Log().Warnf("Session expired after %s", s.manager.Time().Sub(s.lastUpdated))
	s.manager.sessions.remove(s.id)
	s.state = Closed
	for _, watcher := range s.watchers {
		watcher(Closed)
	}
}

func (s *managedSession) scheduleExpireTimer() {
	if s.expireTimer != nil {
		s.expireTimer.Cancel()
	}
	expireTime := s.lastUpdated.Add(s.timeout)
	s.expireTimer = s.manager.Scheduler().Schedule(expireTime, s.expire)
	s.Log().Debugw("Scheduled expire time", logging.Time("Expire", expireTime))
}

func (s *managedSession) open(proposal statemachine.Proposal[*multiraftv1.OpenSessionInput, *multiraftv1.OpenSessionOutput]) {
	defer proposal.Close()
	s.id = ID(proposal.ID())
	s.state = Open
	s.lastUpdated = s.manager.Time()
	s.timeout = proposal.Input().Timeout
	s.log = s.manager.Log().WithFields(logging.Uint64("Session", uint64(s.id)))
	s.manager.sessions.add(s)
	s.scheduleExpireTimer()
	s.Log().Infow("Opened session", logging.Duration("Timeout", s.timeout))
	proposal.Output(&multiraftv1.OpenSessionOutput{
		SessionID: multiraftv1.SessionID(s.ID()),
	})
}

func (s *managedSession) keepAlive(proposal statemachine.Proposal[*multiraftv1.KeepAliveInput, *multiraftv1.KeepAliveOutput]) {
	defer proposal.Close()

	openInputs := &bloom.BloomFilter{}
	if err := json.Unmarshal(proposal.Input().InputFilter, openInputs); err != nil {
		s.Log().Warn("Failed to decode request filter", err)
		proposal.Error(errors.NewInvalid("invalid request filter", err))
		proposal.Close()
		return
	}

	s.Log().Debug("Processing keep-alive")
	for _, p := range s.sessionProposals {
		if proposal.Input().LastInputSequenceNum < p.Input().SequenceNum {
			continue
		}
		sequenceNumBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(sequenceNumBytes, uint64(p.Input().SequenceNum))
		if !openInputs.Test(sequenceNumBytes) {
			p.Cancel()
			delete(s.sessionProposals, p.Input().SequenceNum)
		} else {
			if outputSequenceNum, ok := proposal.Input().LastOutputSequenceNums[p.Input().SequenceNum]; ok {
				p.ack(outputSequenceNum)
			}
		}
	}

	proposal.Output(&multiraftv1.KeepAliveOutput{})

	s.lastUpdated = s.manager.Time()
	s.scheduleExpireTimer()
}

func (s *managedSession) close(proposal statemachine.Proposal[*multiraftv1.CloseSessionInput, *multiraftv1.CloseSessionOutput]) {
	defer proposal.Close()
	s.manager.sessions.remove(s.id)
	s.expireTimer.Cancel()
	s.state = Closed
	for _, watcher := range s.watchers {
		watcher(Closed)
	}
	proposal.Output(&multiraftv1.CloseSessionOutput{})
}

func newManagedSessions() *managedSessions {
	return &managedSessions{
		sessions: make(map[ID]*managedSession),
	}
}

type managedSessions struct {
	sessions map[ID]*managedSession
}

func (s *managedSessions) Get(id ID) (Session, bool) {
	session, ok := s.sessions[id]
	return session, ok
}

func (s *managedSessions) List() []Session {
	sessions := make([]Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

func (s *managedSessions) add(session *managedSession) bool {
	if _, ok := s.sessions[session.ID()]; !ok {
		s.sessions[session.ID()] = session
		return true
	}
	return false
}

func (s *managedSessions) remove(id ID) bool {
	if _, ok := s.sessions[id]; ok {
		delete(s.sessions, id)
		return true
	}
	return false
}

func (s *managedSessions) get(id ID) (*managedSession, bool) {
	session, ok := s.sessions[id]
	return session, ok
}

func (s *managedSessions) list() []*managedSession {
	sessions := make([]*managedSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

func newPrimitiveProposals() *primitiveProposals {
	return &primitiveProposals{
		proposals: make(map[statemachine.ProposalID]*primitiveProposal),
	}
}

type primitiveProposals struct {
	proposals map[statemachine.ProposalID]*primitiveProposal
}

func (p *primitiveProposals) Get(id statemachine.ProposalID) (PrimitiveProposal, bool) {
	proposal, ok := p.proposals[id]
	return proposal, ok
}

func (p *primitiveProposals) List() []PrimitiveProposal {
	proposals := make([]PrimitiveProposal, 0, len(p.proposals))
	for _, proposal := range p.proposals {
		proposals = append(proposals, proposal)
	}
	return proposals
}

func (p *primitiveProposals) add(proposal *primitiveProposal) {
	p.proposals[proposal.ID()] = proposal
}

func (p *primitiveProposals) remove(id statemachine.ProposalID) {
	delete(p.proposals, id)
}

func newSessionProposal(session *managedSession) *sessionProposal {
	return &sessionProposal{
		session: session,
		outputs: list.New(),
	}
}

type sessionProposal struct {
	session      *managedSession
	id           statemachine.ProposalID
	input        *multiraftv1.SessionProposalInput
	timer        statemachine.Timer
	phase        statemachine.Phase
	parent       statemachine.Proposal[*multiraftv1.SessionProposalInput, *multiraftv1.SessionProposalOutput]
	watchers     map[uuid.UUID]statemachine.WatchFunc[statemachine.Phase]
	outputs      *list.List
	outputSeqNum multiraftv1.SequenceNum
	log          logging.Logger
}

func (p *sessionProposal) ID() statemachine.ProposalID {
	return p.id
}

func (p *sessionProposal) Log() logging.Logger {
	return p.log
}

func (p *sessionProposal) Session() Session {
	return p.session
}

func (p *sessionProposal) Watch(watcher statemachine.WatchFunc[statemachine.Phase]) statemachine.CancelFunc {
	if p.watchers == nil {
		p.watchers = make(map[uuid.UUID]statemachine.WatchFunc[statemachine.Phase])
	}
	id := uuid.New()
	p.watchers[id] = watcher
	return func() {
		delete(p.watchers, id)
	}
}

func (p *sessionProposal) execute(parent statemachine.Proposal[*multiraftv1.SessionProposalInput, *multiraftv1.SessionProposalOutput]) {
	p.id = parent.ID()
	p.input = parent.Input()
	p.phase = statemachine.Runnnig
	p.log = p.session.Log().WithFields(logging.Uint64("Proposal", uint64(parent.ID())))
	p.parent = parent
	if parent.Input().Deadline != nil {
		p.timer = p.session.manager.Scheduler().Schedule(*parent.Input().Deadline, p.Cancel)
	}

	switch parent.Input().Input.(type) {
	case *multiraftv1.SessionProposalInput_Proposal:
		proposal := newPrimitiveProposal(p)
		p.session.manager.proposals.add(proposal)
		p.session.proposals.add(proposal)
		p.session.manager.sm.Propose(proposal)
	case *multiraftv1.SessionProposalInput_CreatePrimitive:
		p.session.manager.sm.CreatePrimitive(newCreatePrimitiveProposal(p))
	case *multiraftv1.SessionProposalInput_ClosePrimitive:
		p.session.manager.sm.ClosePrimitive(newClosePrimitiveProposal(p))
	}
}

func (p *sessionProposal) replay(parent statemachine.Proposal[*multiraftv1.SessionProposalInput, *multiraftv1.SessionProposalOutput]) {
	p.parent = parent
	if p.outputs.Len() > 0 {
		p.Log().Debug("Replaying proposal outputs")
		elem := p.outputs.Front()
		for elem != nil {
			output := elem.Value.(*multiraftv1.SessionProposalOutput)
			p.parent.Output(output)
			elem = elem.Next()
		}
	}
	if p.phase == statemachine.Complete {
		p.parent.Close()
	}
}

func (p *sessionProposal) snapshot(writer *snapshot.Writer) error {
	p.Log().Info("Persisting proposal to snapshot")
	pendingOutputs := make([]*multiraftv1.SessionProposalOutput, 0, p.outputs.Len())
	elem := p.outputs.Front()
	for elem != nil {
		pendingOutputs = append(pendingOutputs, elem.Value.(*multiraftv1.SessionProposalOutput))
		elem = elem.Next()
	}

	var phase multiraftv1.SessionProposalSnapshot_Phase
	switch p.phase {
	case statemachine.Pending:
		phase = multiraftv1.SessionProposalSnapshot_PENDING
	case statemachine.Runnnig:
		phase = multiraftv1.SessionProposalSnapshot_RUNNING
	case statemachine.Canceled:
		phase = multiraftv1.SessionProposalSnapshot_CANCELED
	case statemachine.Complete:
		phase = multiraftv1.SessionProposalSnapshot_COMPLETE
	}

	snapshot := &multiraftv1.SessionProposalSnapshot{
		Index:                 multiraftv1.Index(p.ID()),
		Phase:                 phase,
		Input:                 p.input,
		PendingOutputs:        pendingOutputs,
		LastOutputSequenceNum: p.outputSeqNum,
	}
	return writer.WriteMessage(snapshot)
}

func (p *sessionProposal) recover(reader *snapshot.Reader) error {
	snapshot := &multiraftv1.SessionProposalSnapshot{}
	if err := reader.ReadMessage(snapshot); err != nil {
		return err
	}
	p.id = statemachine.ProposalID(snapshot.Index)
	p.input = snapshot.Input
	p.log = p.session.Log().WithFields(logging.Uint64("Proposal", uint64(snapshot.Index)))
	p.Log().Info("Recovering command from snapshot")
	p.outputs = list.New()
	for _, output := range snapshot.PendingOutputs {
		r := output
		p.outputs.PushBack(r)
	}
	p.outputSeqNum = snapshot.LastOutputSequenceNum

	switch snapshot.Phase {
	case multiraftv1.SessionProposalSnapshot_PENDING:
		p.phase = statemachine.Pending
	case multiraftv1.SessionProposalSnapshot_RUNNING:
		p.phase = statemachine.Runnnig
		if p.input.Deadline != nil {
			p.timer = p.session.manager.Scheduler().Schedule(*p.input.Deadline, p.Cancel)
		}
		switch p.input.Input.(type) {
		case *multiraftv1.SessionProposalInput_Proposal:
			proposal := newPrimitiveProposal(p)
			p.session.manager.proposals.add(proposal)
			p.session.proposals.add(proposal)
		}
	case multiraftv1.SessionProposalSnapshot_COMPLETE:
		p.phase = statemachine.Complete
	case multiraftv1.SessionProposalSnapshot_CANCELED:
		p.phase = statemachine.Canceled
	}
	return nil
}

func (p *sessionProposal) ack(outputSequenceNum multiraftv1.SequenceNum) {
	p.Log().Debugw("Acked proposal outputs",
		logging.Uint64("SequenceNum", uint64(outputSequenceNum)))
	elem := p.outputs.Front()
	for elem != nil && elem.Value.(*multiraftv1.SessionProposalOutput).SequenceNum <= outputSequenceNum {
		next := elem.Next()
		p.outputs.Remove(elem)
		elem = next
	}
}

func (p *sessionProposal) nextSequenceNum() multiraftv1.SequenceNum {
	p.outputSeqNum++
	return p.outputSeqNum
}

func (p *sessionProposal) Input() *multiraftv1.SessionProposalInput {
	return p.input
}

func (p *sessionProposal) Output(output *multiraftv1.SessionProposalOutput) {
	if p.phase == statemachine.Complete {
		return
	}
	p.Log().Debugw("Cached command output", logging.Uint64("SequenceNum", uint64(output.SequenceNum)))
	p.outputs.PushBack(output)
	if p.parent != nil {
		p.parent.Output(output)
	}
}

func (p *sessionProposal) Error(err error) {
	if p.phase == statemachine.Complete {
		return
	}
	p.Output(&multiraftv1.SessionProposalOutput{
		SequenceNum: p.nextSequenceNum(),
		Failure:     getFailure(err),
	})
}

func (p *sessionProposal) Close() {
	if p.phase != statemachine.Runnnig {
		return
	}
	if p.parent != nil {
		p.parent.Close()
	}
	p.close(statemachine.Complete)
}

func (p *sessionProposal) Cancel() {
	if p.phase != statemachine.Runnnig {
		return
	}
	if p.parent != nil {
		p.parent.Cancel()
	}
	p.close(statemachine.Canceled)
}

func (p *sessionProposal) close(phase statemachine.Phase) {
	p.phase = phase
	p.session.manager.proposals.remove(p.id)
	p.session.proposals.remove(p.id)
	if p.timer != nil {
		p.timer.Cancel()
	}
	if p.watchers != nil {
		for _, watcher := range p.watchers {
			watcher(phase)
		}
	}
}

func newPrimitiveProposal(parent *sessionProposal) *primitiveProposal {
	return &primitiveProposal{
		sessionProposal: parent,
	}
}

type primitiveProposal struct {
	*sessionProposal
}

func (p *primitiveProposal) Input() *multiraftv1.PrimitiveProposalInput {
	return p.sessionProposal.Input().GetProposal()
}

func (p *primitiveProposal) Output(output *multiraftv1.PrimitiveProposalOutput) {
	p.sessionProposal.Output(&multiraftv1.SessionProposalOutput{
		SequenceNum: p.nextSequenceNum(),
		Output: &multiraftv1.SessionProposalOutput_Proposal{
			Proposal: output,
		},
	})
}

var _ Proposal[*multiraftv1.PrimitiveProposalInput, *multiraftv1.PrimitiveProposalOutput] = (*primitiveProposal)(nil)

func newCreatePrimitiveProposal(parent *sessionProposal) *createPrimitiveProposal {
	return &createPrimitiveProposal{
		sessionProposal: parent,
	}
}

type createPrimitiveProposal struct {
	*sessionProposal
}

func (p *createPrimitiveProposal) Input() *multiraftv1.CreatePrimitiveInput {
	return p.sessionProposal.Input().GetCreatePrimitive()
}

func (p *createPrimitiveProposal) Output(output *multiraftv1.CreatePrimitiveOutput) {
	p.sessionProposal.Output(&multiraftv1.SessionProposalOutput{
		SequenceNum: p.nextSequenceNum(),
		Output: &multiraftv1.SessionProposalOutput_CreatePrimitive{
			CreatePrimitive: output,
		},
	})
}

var _ Proposal[*multiraftv1.CreatePrimitiveInput, *multiraftv1.CreatePrimitiveOutput] = (*createPrimitiveProposal)(nil)

func newClosePrimitiveProposal(parent *sessionProposal) *closePrimitiveProposal {
	return &closePrimitiveProposal{
		sessionProposal: parent,
	}
}

type closePrimitiveProposal struct {
	*sessionProposal
}

func (p *closePrimitiveProposal) Input() *multiraftv1.ClosePrimitiveInput {
	return p.sessionProposal.Input().GetClosePrimitive()
}

func (p *closePrimitiveProposal) Output(output *multiraftv1.ClosePrimitiveOutput) {
	p.sessionProposal.Output(&multiraftv1.SessionProposalOutput{
		SequenceNum: p.nextSequenceNum(),
		Output: &multiraftv1.SessionProposalOutput_ClosePrimitive{
			ClosePrimitive: output,
		},
	})
}

var _ Proposal[*multiraftv1.ClosePrimitiveInput, *multiraftv1.ClosePrimitiveOutput] = (*closePrimitiveProposal)(nil)

func newSessionQuery(session *managedSession) *sessionQuery {
	return &sessionQuery{
		session: session,
	}
}

type sessionQuery struct {
	session *managedSession
	parent  statemachine.Query[*multiraftv1.SessionQueryInput, *multiraftv1.SessionQueryOutput]
	timer   statemachine.Timer
	log     logging.Logger
}

func (q *sessionQuery) ID() statemachine.QueryID {
	return q.parent.ID()
}

func (q *sessionQuery) Log() logging.Logger {
	return q.log
}

func (q *sessionQuery) Session() Session {
	return q.session
}

func (q *sessionQuery) Watch(watcher statemachine.WatchFunc[statemachine.Phase]) statemachine.CancelFunc {
	return q.parent.Watch(watcher)
}

func (q *sessionQuery) execute(parent statemachine.Query[*multiraftv1.SessionQueryInput, *multiraftv1.SessionQueryOutput]) {
	q.parent = parent
	q.log = q.session.Log().WithFields(logging.Uint64("Query", uint64(parent.ID())))
	if parent.Input().Deadline != nil {
		q.timer = q.session.manager.Scheduler().Schedule(*parent.Input().Deadline, q.Cancel)
	}
	switch parent.Input().Input.(type) {
	case *multiraftv1.SessionQueryInput_Query:
		query := newPrimitiveQuery(q)
		q.session.manager.sm.Query(query)
	}
}

func (q *sessionQuery) Input() *multiraftv1.SessionQueryInput {
	return q.parent.Input()
}

func (q *sessionQuery) Output(output *multiraftv1.SessionQueryOutput) {
	q.parent.Output(output)
}

func (q *sessionQuery) Error(err error) {
	q.parent.Error(err)
}

func (q *sessionQuery) Cancel() {
	if q.timer != nil {
		q.timer.Cancel()
	}
	q.parent.Cancel()
}

func (q *sessionQuery) Close() {
	if q.timer != nil {
		q.timer.Cancel()
	}
	q.parent.Close()
}

var _ Query[*multiraftv1.SessionQueryInput, *multiraftv1.SessionQueryOutput] = (*sessionQuery)(nil)

func newPrimitiveQuery(parent *sessionQuery) *primitiveQuery {
	return &primitiveQuery{
		sessionQuery: parent,
	}
}

type primitiveQuery struct {
	*sessionQuery
}

func (p *primitiveQuery) Input() *multiraftv1.PrimitiveQueryInput {
	return p.sessionQuery.Input().GetQuery()
}

func (p *primitiveQuery) Output(output *multiraftv1.PrimitiveQueryOutput) {
	p.sessionQuery.Output(&multiraftv1.SessionQueryOutput{
		Output: &multiraftv1.SessionQueryOutput_Query{
			Query: output,
		},
	})
}

func (p *primitiveQuery) Error(err error) {
	p.sessionQuery.Output(&multiraftv1.SessionQueryOutput{
		Failure: getFailure(err),
	})
}

var _ Query[*multiraftv1.PrimitiveQueryInput, *multiraftv1.PrimitiveQueryOutput] = (*primitiveQuery)(nil)

// getFailure gets the proto status for the given error
func getFailure(err error) *multiraftv1.Failure {
	if err == nil {
		return nil
	}
	return &multiraftv1.Failure{
		Status:  getStatus(err),
		Message: getMessage(err),
	}
}

func getStatus(err error) multiraftv1.Failure_Status {
	typed, ok := err.(*errors.TypedError)
	if !ok {
		return multiraftv1.Failure_ERROR
	}

	switch typed.Type {
	case errors.Unknown:
		return multiraftv1.Failure_UNKNOWN
	case errors.Canceled:
		return multiraftv1.Failure_CANCELED
	case errors.NotFound:
		return multiraftv1.Failure_NOT_FOUND
	case errors.AlreadyExists:
		return multiraftv1.Failure_ALREADY_EXISTS
	case errors.Unauthorized:
		return multiraftv1.Failure_UNAUTHORIZED
	case errors.Forbidden:
		return multiraftv1.Failure_FORBIDDEN
	case errors.Conflict:
		return multiraftv1.Failure_CONFLICT
	case errors.Invalid:
		return multiraftv1.Failure_INVALID
	case errors.Unavailable:
		return multiraftv1.Failure_UNAVAILABLE
	case errors.NotSupported:
		return multiraftv1.Failure_NOT_SUPPORTED
	case errors.Timeout:
		return multiraftv1.Failure_TIMEOUT
	case errors.Fault:
		return multiraftv1.Failure_FAULT
	case errors.Internal:
		return multiraftv1.Failure_INTERNAL
	default:
		return multiraftv1.Failure_ERROR
	}
}

// getMessage gets the message for the given error
func getMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
