package rafted

import (
    "errors"
    hsm "github.com/hhkbp2/go-hsm"
    cm "github.com/hhkbp2/rafted/comm"
    ev "github.com/hhkbp2/rafted/event"
    logging "github.com/hhkbp2/rafted/logging"
    ps "github.com/hhkbp2/rafted/persist"
    rt "github.com/hhkbp2/rafted/retry"
    "io"
    "time"
)

var (
    Success             error = nil
    Failure                   = errors.New("failure")
    Timeout                   = errors.New("timeout")
    LeaderUnsync              = errors.New("leader unsync")
    LeaderUnknown             = errors.New("leader Unknown")
    InMemberChange            = errors.New("in member change")
    PersistError              = errors.New("persist error")
    InvalidResponseType       = errors.New("invalid response type")
)

type Client interface {
    Append(data []byte) (result []byte, err error)
    ReadOnly(data []byte) (result []byte, err error)
    GetConfig() (conf *ps.Config, err error)
    ChangeConfig(conf *ps.Config) error
    io.Closer
}

type SimpleClient struct {
    backend Backend
    timeout time.Duration
    retry   rt.Retry
}

func NewSimpleClient(
    backend Backend, timeout time.Duration, retry rt.Retry) *SimpleClient {

    return &SimpleClient{
        backend: backend,
        timeout: timeout,
        retry:   retry,
    }
}

func (self *SimpleClient) Append(data []byte) (result []byte, err error) {
    request := &ev.ClientAppendRequest{
        Data: data,
    }
    reqEvent := ev.NewClientAppendRequestEvent(request)
    return doRequest(self.backend, reqEvent, self.timeout, self.retry,
        self.retry, dummyRedirectHandler)
}

func (self *SimpleClient) ReadOnly(data []byte) (result []byte, err error) {
    request := &ev.ClientReadOnlyRequest{
        Data: data,
    }
    reqEvent := ev.NewClientReadOnlyRequestEvent(request)
    return doRequest(self.backend, reqEvent, self.timeout, self.retry,
        self.retry, dummyRedirectHandler)

}

func (self *SimpleClient) GetConfig() (conf *ps.Config, err error) {
    // TODO add impl
    return nil, nil
}

func (self *SimpleClient) ChangeConfig(conf *ps.Config) error {
    request := &ev.ClientChangeConfigRequest{
        Conf: conf,
    }
    reqEvent := ev.NewClientChangeConfigRequestEvent(request)
    _, err := doRequest(self.backend, reqEvent, self.timeout, self.retry,
        self.retry, dummyRedirectHandler)
    return err
}

func (self *SimpleClient) Close() error {
    // empty body
    return nil
}

type RedirectClient struct {
    timeout       time.Duration
    retry         rt.Retry
    redirectRetry rt.Retry

    backend Backend
    client  cm.Client
    server  cm.Server
    logger  logging.Logger
}

func NewRedirectClient(
    timeout time.Duration,
    retry rt.Retry,
    redirectRetry rt.Retry,
    backend Backend,
    client cm.Client,
    server cm.Server,
    logger logging.Logger) *RedirectClient {

    return &RedirectClient{
        timeout:       timeout,
        retry:         retry,
        redirectRetry: redirectRetry,
        backend:       backend,
        client:        client,
        server:        server,
        logger:        logger,
    }
}

func (self *RedirectClient) Start() error {
    self.server.Serve()
    return nil
}

func (self *RedirectClient) Close() error {
    return self.server.Close()
}

func (self *RedirectClient) genRedirectHandler() RedirectResponseHandler {
    return func(
        respEvent *ev.LeaderRedirectResponseEvent,
        reqEvent ev.RequestEvent) (ev.Event, error) {

        self.logger.Debug("redirect to leader: %s", respEvent.Response.Leader)
        return self.client.CallRPCTo(respEvent.Response.Leader, reqEvent)
    }
}

func (self *RedirectClient) Append(data []byte) (result []byte, err error) {
    request := &ev.ClientAppendRequest{
        Data: data,
    }
    reqEvent := ev.NewClientAppendRequestEvent(request)
    return doRequest(self.backend, reqEvent, self.timeout, self.retry,
        self.redirectRetry, self.genRedirectHandler())
}

func (self *RedirectClient) ReadOnly(data []byte) (result []byte, err error) {
    request := &ev.ClientReadOnlyRequest{
        Data: data,
    }
    reqEvent := ev.NewClientReadOnlyRequestEvent(request)
    return doRequest(self.backend, reqEvent, self.timeout, self.retry,
        self.redirectRetry, self.genRedirectHandler())
}

func (self *RedirectClient) GetConfig() (conf *ps.Config, err error) {
    // TODO add impl
    return nil, nil
}

func (self *RedirectClient) ChangeConfig(conf *ps.Config) error {
    request := &ev.ClientChangeConfigRequest{
        Conf: conf,
    }
    reqEvent := ev.NewClientChangeConfigRequestEvent(request)
    _, err := doRequest(self.backend, reqEvent, self.timeout, self.retry,
        self.redirectRetry, self.genRedirectHandler())
    return err
}

func sendToBackend(
    backend Backend,
    reqEvent ev.RequestEvent,
    timeout time.Duration) (event ev.Event, err error) {

    backend.Send(reqEvent)
    timeChan := time.After(timeout)
    select {
    case event := <-reqEvent.GetResponseChan():
        return event, nil
    case <-timeChan:
        return nil, Timeout
    }
}

type RedirectResponseHandler func(
    *ev.LeaderRedirectResponseEvent, ev.RequestEvent) (ev.Event, error)

type InavaliableResponseHandler func(ev.Event, ev.RequestEvent) ([]byte, error)

func doRequest(
    backend Backend,
    reqEvent ev.RequestEvent,
    timeout time.Duration,
    retry rt.Retry,
    redirectRetry rt.Retry,
    redirectHandler RedirectResponseHandler) ([]byte, error) {

    resultChan := make(chan []byte, 1)
    fn := func() error {
        respEvent, err := sendToBackend(backend, reqEvent, timeout)
        if err != nil {
            return err
        }
        if respEvent.Type() == ev.EventLeaderRedirectResponse {
            redirect := func() error {
                e, ok := respEvent.(*ev.LeaderRedirectResponseEvent)
                hsm.AssertTrue(ok)
                respEvent, err = redirectHandler(e, reqEvent)
                return err
            }
            err = redirectRetry.Do(redirect)
            if err != nil {
                return err
            }
        }
        switch respEvent.Type() {
        case ev.EventClientResponse:
            e, ok := respEvent.(*ev.ClientResponseEvent)
            hsm.AssertTrue(ok)
            if e.Response.Success {
                resultChan <- e.Response.Data
                return nil
            }
            return Failure
        case ev.EventLeaderUnknownResponse:
            return LeaderUnknown
        case ev.EventLeaderUnsyncResponse:
            return LeaderUnsync
        case ev.EventLeaderInMemberChangeResponse:
            return InMemberChange
        case ev.EventPersistErrorResponse:
            return PersistError
        default:
            return InvalidResponseType
        }
    }

    err := retry.Do(fn)
    if err != nil {
        return nil, err
    }
    result := <-resultChan
    return result, nil
}

func dummyRedirectHandler(
    respEvent *ev.LeaderRedirectResponseEvent,
    _ ev.RequestEvent) (ev.Event, error) {

    return respEvent, nil
}
