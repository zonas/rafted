package rafted

import (
    "errors"
    "fmt"
    hsm "github.com/hhkbp2/go-hsm"
    ev "github.com/hhkbp2/rafted/event"
    logging "github.com/hhkbp2/rafted/logging"
    ps "github.com/hhkbp2/rafted/persist"
    "io"
    "sync"
    "sync/atomic"
    "time"
)

const (
    HSMTypeLocal hsm.HSMType = hsm.HSMTypeStd + 1 + iota
    HSMTypePeer
    HSMTypeLeaderMemberChange
)

type MemberChangeStatusType uint8

const (
    MemberChangeStatusNotSet MemberChangeStatusType = iota
    NotInMemeberChange
    OldNewConfigSeen
    OldNewConfigCommitted
    NewConfigSeen
    NewConfigCommitted
)

type SelfDispatchHSM interface {
    hsm.HSM
    SelfDispatch(event hsm.Event)
}

type TerminableHSM interface {
    hsm.HSM
    Terminate()
}

type NotifiableHSM interface {
    hsm.HSM
    GetNotifyChan() <-chan ev.NotifyEvent
}

type LocalHSM struct {
    *hsm.StdHSM
    dispatchChan     *ReliableEventChannel
    selfDispatchChan *ReliableEventChannel
    stopChan         chan interface{}
    group            sync.WaitGroup

    // the current term
    currentTerm uint64

    // the local addr
    localAddr     *ps.ServerAddress
    localAddrLock sync.RWMutex

    // CandidateId that received vote in current term(or nil if none)
    votedFor     *ps.ServerAddress
    votedForLock sync.RWMutex
    // leader infos
    leader     *ps.ServerAddress
    leaderLock sync.RWMutex

    // member change infos
    memberChangeStatus     MemberChangeStatusType
    memberChangeStatusLock sync.RWMutex

    log           ps.Log
    stateMachine  ps.StateMachine
    configManager ps.ConfigManager

    applier  *Applier
    peers    Peers
    notifier *Notifier
    logging.Logger
}

func NewLocalHSM(
    top hsm.State,
    initial hsm.State,
    localAddr *ps.ServerAddress,
    configManager ps.ConfigManager,
    stateMachine ps.StateMachine,
    log ps.Log,
    logger logging.Logger) (*LocalHSM, error) {

    // TODO check the integrety between log and configManager
    // try to fix the inconsistant base on log status.
    // Return error if fail to do that

    memberChangeStatus, err := InitMemberChangeStatus(configManager, log)
    if err != nil {
        logger.Error("fail to initialize member change status")
        return nil, err
    }

    term, err := log.LastTerm()
    if err != nil {
        logger.Error("fail to read last entry term of log")
        return nil, err
    }

    notifier := NewNotifier()
    object := &LocalHSM{
        // hsm
        StdHSM:           hsm.NewStdHSM(HSMTypeLocal, top, initial),
        dispatchChan:     NewReliableEventChannel(),
        selfDispatchChan: NewReliableEventChannel(),
        stopChan:         make(chan interface{}, 1),
        group:            sync.WaitGroup{},
        // additional fields
        currentTerm:        term,
        localAddr:          localAddr,
        memberChangeStatus: memberChangeStatus,
        log:                log,
        stateMachine:       stateMachine,
        configManager:      configManager,
        notifier:           notifier,
        Logger:             logger,
    }

    dispatcher := func(event hsm.Event) {
        object.SelfDispatch(event)
    }
    applier := NewApplier(log, stateMachine, dispatcher, notifier, logger)
    object.SetApplier(applier)
    return object, nil
}

func (self *LocalHSM) Init() {
    self.StdHSM.Init2(self, hsm.NewStdEvent(hsm.EventInit))
    self.loop()
}

func (self *LocalHSM) loop() {
    routine := func() {
        defer self.group.Done()
        // loop forever to process incoming event
        priorityChan := self.selfDispatchChan.GetOutChan()
        eventChan := self.dispatchChan.GetOutChan()
        for {
            // Event in selfDispatchChan has higher priority to be processed
            select {
            case <-self.stopChan:
                return
            case event := <-priorityChan:
                self.StdHSM.Dispatch2(self, event)
                continue
            case <-time.After(0):
                // no event in priorityChan
            }
            select {
            case <-self.stopChan:
                return
            case event := <-priorityChan:
                self.StdHSM.Dispatch2(self, event)
            case event := <-eventChan:
                self.StdHSM.Dispatch2(self, event)
            }
        }
    }
    self.group.Add(1)
    go routine()
}

func (self *LocalHSM) Dispatch(event hsm.Event) {
    self.dispatchChan.Send(event)
}

func (self *LocalHSM) QTran(targetStateID string) {
    target := self.StdHSM.LookupState(targetStateID)
    self.StdHSM.QTranHSM(self, target)
}

func (self *LocalHSM) QTranOnEvent(targetStateID string, event hsm.Event) {
    target := self.StdHSM.LookupState(targetStateID)
    self.StdHSM.QTranHSMOnEvent(self, target, event)
}

func (self *LocalHSM) SelfDispatch(event hsm.Event) {
    self.selfDispatchChan.Send(event)
}

func (self *LocalHSM) Terminate() {
    self.SelfDispatch(hsm.NewStdEvent(ev.EventTerm))
    self.stopChan <- self
    self.group.Wait()
    self.dispatchChan.Close()
    self.selfDispatchChan.Close()
    self.applier.Close()
    self.notifier.Close()
}

func (self *LocalHSM) GetCurrentTerm() uint64 {
    return atomic.LoadUint64(&self.currentTerm)
}

func (self *LocalHSM) SetCurrentTerm(term uint64) {
    atomic.StoreUint64(&self.currentTerm, term)
}

func (self *LocalHSM) SetCurrentTermWithNotify(term uint64) {
    oldTerm := self.GetCurrentTerm()
    self.SetCurrentTerm(term)
    self.Notifier().Notify(ev.NewNotifyTermChangeEvent(oldTerm, term))
}

func (self *LocalHSM) GetLocalAddr() *ps.ServerAddress {
    self.localAddrLock.RLock()
    defer self.localAddrLock.RUnlock()
    return self.localAddr
}

func (self *LocalHSM) SetLocalAddr(addr *ps.ServerAddress) {
    self.localAddrLock.Lock()
    defer self.localAddrLock.Unlock()
    self.localAddr = addr
}

func (self *LocalHSM) GetVotedFor() *ps.ServerAddress {
    self.votedForLock.RLock()
    defer self.votedForLock.RUnlock()
    return self.votedFor
}

func (self *LocalHSM) SetVotedFor(votedFor *ps.ServerAddress) {
    self.votedForLock.Lock()
    defer self.votedForLock.Unlock()
    self.votedFor = votedFor
}

func (self *LocalHSM) GetLeader() *ps.ServerAddress {
    self.leaderLock.RLock()
    defer self.leaderLock.RUnlock()
    return self.leader
}

func (self *LocalHSM) SetLeader(leader *ps.ServerAddress) {
    self.leaderLock.Lock()
    defer self.leaderLock.Unlock()
    self.leader = leader
}

func (self *LocalHSM) SetLeaderWithNotify(leader *ps.ServerAddress) {
    self.SetLeader(leader)
    self.Notifier().Notify(ev.NewNotifyLeaderChangeEvent(leader))
}

func (self *LocalHSM) GetMemberChangeStatus() MemberChangeStatusType {
    self.memberChangeStatusLock.RLock()
    defer self.memberChangeStatusLock.RUnlock()
    return self.memberChangeStatus
}

func (self *LocalHSM) SetMemberChangeStatus(status MemberChangeStatusType) {
    self.memberChangeStatusLock.Lock()
    defer self.memberChangeStatusLock.Unlock()
    self.memberChangeStatus = status
}

func (self *LocalHSM) SendMemberChangeNotify() error {
    conf, err := self.configManager.RNth(2)
    if err != nil {
        return err
    }
    if !conf.IsOldNewConfig() {
        return errors.New("config data corrupted")
    }
    self.Notifier().Notify(ev.NewNotifyMemberChangeEvent(
        conf.Servers, conf.NewServers))
    return nil
}

func (self *LocalHSM) ConfigManager() ps.ConfigManager {
    return self.configManager
}

func (self *LocalHSM) Log() ps.Log {
    return self.log
}

func (self *LocalHSM) StateMachine() ps.StateMachine {
    return self.stateMachine
}

func (self *LocalHSM) Peers() Peers {
    return self.peers
}

func (self *LocalHSM) SetPeers(peers Peers) {
    self.peers = peers
}

func (self *LocalHSM) SetApplier(applier *Applier) {
    self.applier = applier
}

func (self *LocalHSM) Notifier() *Notifier {
    return self.notifier
}

func (self *LocalHSM) CommitLogsUpTo(index uint64) error {
    committedIndex, err := self.log.CommittedIndex()
    if err != nil {
        return err
    }
    if index < committedIndex {
        return errors.New("index less than committed index")
    }
    if index == committedIndex {
        return nil
    }
    if err = self.log.StoreCommittedIndex(index); err != nil {
        return err
    }
    self.applier.FollowerCommitUpTo(index)
    return nil
}

func (self *LocalHSM) CommitInflightLog(entry *InflightEntry) error {
    committedIndex, err := self.log.CommittedIndex()
    if err != nil {
        message := fmt.Sprintf(
            "fail to read committed index of log, error: %s", err)
        return errors.New(message)
    }
    logIndex := entry.Request.LogEntry.Index
    if logIndex <= committedIndex {
        return errors.New("index less than committed index")
    }
    if logIndex != committedIndex+1 {
        return errors.New("index not next to last committed index")
    }
    if err = self.log.StoreCommittedIndex(logIndex); err != nil {
        message := fmt.Sprintf(
            "fail to store committed index of log, error: %s", err)
        return errors.New(message)
    }
    self.Debug("** commit inflight log at index: %d to applier", logIndex)
    self.applier.LeaderCommit(entry)
    return nil
}

func InitMemberChangeStatus(
    configManager ps.ConfigManager,
    log ps.Log) (MemberChangeStatusType, error) {

    // setup member change status according to the recent config
    committedIndex, err := log.CommittedIndex()
    if err != nil {
        return MemberChangeStatusNotSet, err
    }
    metas, err := configManager.ListAfter(committedIndex)
    if err != nil {
        return MemberChangeStatusNotSet, err
    }
    if len(metas) == 0 {
        return MemberChangeStatusNotSet, errors.New(fmt.Sprintf(
            "no config after committed index %d", committedIndex))
    }
    length := len(metas)
    if length > 2 {
        return MemberChangeStatusNotSet, errors.New(fmt.Sprintf(
            "%d configs after committed index %d", length, committedIndex))
    }
    if length == 1 {
        conf := metas[0].Conf
        if conf.IsNormalConfig() {
            return NotInMemeberChange, nil
        } else if conf.IsOldNewConfig() {
            return OldNewConfigCommitted, nil
        }
    } else { // length == 2
        prevConf := metas[0].Conf
        nextConf := metas[1].Conf
        if prevConf.IsNormalConfig() && nextConf.IsOldNewConfig() {
            return OldNewConfigSeen, nil
        } else if prevConf.IsOldNewConfig() && nextConf.IsNewConfig() {
            return NewConfigSeen, nil
        } else if prevConf.IsNewConfig() && nextConf.IsNormalConfig() {
            return NotInMemeberChange, nil
        }
    }
    return MemberChangeStatusNotSet, errors.New(fmt.Sprintf(
        "corrupted config after committed index %d", committedIndex))
}

type Local interface {
    Send(event hsm.Event)
    SendPrior(event hsm.Event)
    io.Closer

    QueryState() string
    GetCurrentTerm() uint64
    GetLocalAddr() *ps.ServerAddress
    GetVotedFor() *ps.ServerAddress
    GetLeader() *ps.ServerAddress

    Log() ps.Log
    StateMachine() ps.StateMachine
    ConfigManager() ps.ConfigManager
    Notifier() *Notifier

    SetPeers(peers Peers)
}

type LocalManager struct {
    localHSM *LocalHSM
}

func NewLocalManager(
    config *Configuration,
    localAddr *ps.ServerAddress,
    log ps.Log,
    stateMachine ps.StateMachine,
    configManager ps.ConfigManager,
    logger logging.Logger) (Local, error) {

    top := hsm.NewTop()
    initial := hsm.NewInitial(top, StateLocalID)
    localState := NewLocalState(top, logger)
    followerState := NewFollowerState(
        localState,
        config.ElectionTimeout,
        config.ElectionTimeoutThresholdPersent,
        config.MaxTimeoutJitter,
        logger)
    NewSnapshotRecoveryState(followerState, logger)
    followerMemberChangeState := NewFollowerMemberChangeState(
        followerState, logger)
    NewFollowerOldNewConfigSeenState(followerMemberChangeState, logger)
    NewFollowerOldNewConfigCommittedState(followerMemberChangeState, logger)
    NewFollowerNewConfigSeenState(followerMemberChangeState, logger)
    needPeersState := NewNeedPeersState(localState, logger)
    NewCandidateState(
        needPeersState, config.ElectionTimeout, config.MaxTimeoutJitter, logger)
    leaderState := NewLeaderState(needPeersState, logger)
    NewUnsyncState(leaderState, logger)
    NewSyncState(leaderState, logger)
    NewPersistErrorState(localState, config.PersistErrorNotifyTimeout, logger)
    hsm.NewTerminal(top)
    localHSM, err := NewLocalHSM(
        top,
        initial,
        localAddr,
        configManager,
        stateMachine,
        log,
        logger)
    if err != nil {
        return nil, err
    }
    localHSM.Init()
    return &LocalManager{localHSM}, nil
}

func (self *LocalManager) Send(event hsm.Event) {
    self.localHSM.Dispatch(event)
}

func (self *LocalManager) SendPrior(event hsm.Event) {
    self.localHSM.SelfDispatch(event)
}

func (self *LocalManager) Close() error {
    self.localHSM.Terminate()
    return nil
}

func (self *LocalManager) QueryState() string {
    requestEvent := ev.NewQueryStateRequestEvent()
    self.localHSM.Dispatch(requestEvent)
    responseEvent := requestEvent.RecvResponse()
    hsm.AssertEqual(responseEvent.Type(), ev.EventQueryStateResponse)
    event, ok := responseEvent.(*ev.QueryStateResponseEvent)
    hsm.AssertTrue(ok)
    return event.Response.StateID
}

func (self *LocalManager) GetCurrentTerm() uint64 {
    return self.localHSM.GetCurrentTerm()
}

func (self *LocalManager) GetLocalAddr() *ps.ServerAddress {
    return self.localHSM.GetLocalAddr()
}

func (self *LocalManager) GetVotedFor() *ps.ServerAddress {
    return self.localHSM.GetVotedFor()
}

func (self *LocalManager) GetLeader() *ps.ServerAddress {
    return self.localHSM.GetLeader()
}

func (self *LocalManager) Log() ps.Log {
    return self.localHSM.Log()
}

func (self *LocalManager) StateMachine() ps.StateMachine {
    return self.localHSM.StateMachine()
}

func (self *LocalManager) ConfigManager() ps.ConfigManager {
    return self.localHSM.ConfigManager()
}

func (self *LocalManager) Notifier() *Notifier {
    return self.localHSM.Notifier()
}

func (self *LocalManager) SetPeers(peers Peers) {
    self.localHSM.SetPeers(peers)
}
