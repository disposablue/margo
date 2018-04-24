package mg

import (
	"bufio"
	"fmt"
	"github.com/ugorji/go/codec"
	"io"
	"margo.sh/mgutil"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// DefaultCodec is the name of the default codec used for IPC communication
	DefaultCodec = "json"

	// codecHandles is the map of all valid codec handles
	codecHandles = func() map[string]codec.Handle {
		m := map[string]codec.Handle{
			"cbor": &codec.CborHandle{},
			"json": &codec.JsonHandle{
				Indent:         2,
				TermWhitespace: true,
			},
			"msgpack": &codec.MsgpackHandle{},
		}
		m[""] = m[DefaultCodec]
		return m
	}()

	// CodecNames is the list of names of all valid codec handles
	CodecNames = func() []string {
		l := make([]string, 0, len(codecHandles))
		for k, _ := range codecHandles {
			if k != "" {
				l = append(l, k)
			}
		}
		sort.Strings(l)
		return l
	}()

	// CodecNamesStr is the list of names of all valid codec handles in the form `a, b or c`
	CodecNamesStr = func() string {
		i := len(CodecNames) - 1
		return strings.Join(CodecNames[:i], ", ") + " or " + CodecNames[i]

	}()
)

type AgentConfig struct {
	// the name of the agent as used in the command `margo.sh [start...] $AgentName`
	AgentName string

	// Codec is the name of the codec to use for IPC
	// Valid values are json, cbor or msgpack
	// Default: json
	Codec string

	// Stdin is the stream through which the client sends encoded request data
	// It's closed when Agent.Run() returns
	Stdin io.ReadCloser

	// Stdout is the stream through which the server (the Agent type) sends encoded responses
	// It's closed when Agent.Run() returns
	Stdout io.WriteCloser

	// Stderr is used for logging
	// Clients are encouraged to leave it open until the process exits
	// to allow for logging to keep working during process shutdown
	Stderr io.Writer
}

type agentReqAction struct {
	Name string
	Data codec.Raw
}

type agentReq struct {
	Cookie  string
	Actions []agentReqAction
	Props   clientProps
}

func newAgentReq(kvs KVStore) *agentReq {
	return &agentReq{Props: makeClientProps(kvs)}
}

func (rq *agentReq) finalize(ag *Agent) {
	rq.Props.finalize(ag)
}

type agentRes struct {
	Cookie string
	Error  string
	State  *State
}

func (rs agentRes) finalize() interface{} {
	type _ReducerProfile struct {
		ReducerProfile
		Action string
		Start  string
		End    string
	}

	out := struct {
		agentRes
		State struct {
			State
			Config        interface{}
			ClientActions []clientActionType
			Profiles      []_ReducerProfile
		}
	}{}
	out.agentRes = rs
	out.State.State = *rs.State
	inSt := &out.State.State
	outSt := &out.State

	outSt.Profiles = make([]_ReducerProfile, len(inSt.Profiles))
	for i, out := range outSt.Profiles {
		in := inSt.Profiles[i]
		out.ReducerProfile = in
		if t := reflect.TypeOf(in.Action); t != nil {
			out.Action = t.String()
		} else {
			out.Action = "mg.Render"
		}
		out.Start = in.Start.Format(time.RFC3339Nano)
		out.End = in.End.Format(time.RFC3339Nano)
		outSt.Profiles[i] = out
	}

	outSt.ClientActions = inSt.clientActions

	if out.Error == "" {
		out.Error = strings.Join([]string(outSt.Errors), "\n")
	}

	if outSt.View.changed == 0 {
		outSt.View = nil
	}

	if ec := inSt.Config; ec != nil {
		outSt.Config = ec.EditorConfig()
	}

	return out
}

type Agent struct {
	Name  string
	Done  <-chan struct{}
	Log   *Logger
	Store *Store

	mu sync.Mutex

	stdin  io.ReadCloser
	stdout io.WriteCloser
	stderr io.Writer

	handle codec.Handle
	enc    *codec.Encoder
	encWr  *bufio.Writer
	dec    *codec.Decoder
	wg     sync.WaitGroup

	sd struct {
		mu     sync.Mutex
		done   chan<- struct{}
		closed bool
	}
	closed bool
}

// Run starts the Agent's event loop. It returns immediately on the first error.
func (ag *Agent) Run() error {
	defer ag.shutdown()
	return ag.communicate()
}

func (ag *Agent) communicate() error {
	ag.Log.Println("started")
	ag.Store.dispatch(Started{})
	ag.Store.ready()

	for {
		rq := newAgentReq(ag.Store)
		if err := ag.dec.Decode(rq); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("ipc.decode: %s", err)
		}
		rq.finalize(ag)
		ag.handleReq(rq)
	}
}

func (ag *Agent) handleReq(rq *agentReq) {
	ag.wg.Add(1)
	defer ag.wg.Done()

	// TODO: put this on a channel in the future.
	// at the moment we lock the store and block new requests to maintain request/response order
	// but decoding time could become a problem if we start sending large requests from the client
	// we currently only have 1 client (GoSublime) that we also control so it's ok for now...

	ag.Store.syncRq(ag, rq)
}

func (ag *Agent) createAction(ra agentReqAction, h codec.Handle) (Action, error) {
	if f := actionCreators[ra.Name]; f != nil {
		return f(h, ra)
	}
	return nil, fmt.Errorf("Unknown action: %s", ra.Name)
}

func (ag *Agent) listener(st *State) {
	err := ag.send(agentRes{State: st})
	if err != nil {
		ag.Log.Println("agent.send failed. shutting down ipc:", err)
		go ag.shutdown()
	}
}

func (ag *Agent) send(res agentRes) error {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	defer ag.encWr.Flush()
	return ag.enc.Encode(res.finalize())
}

// shutdown sequence:
// * stop incoming requests
// * wait for all reqs to complete
// * tell reducers we're shutting down
// * stop outgoing responses
// * tell the world we're done
func (ag *Agent) shutdown() {
	sd := &ag.sd
	sd.mu.Lock()
	defer sd.mu.Unlock()

	if sd.closed {
		return
	}
	sd.closed = true

	// defers because we want *some* guarantee that all these steps will be taken
	defer close(sd.done)
	defer ag.stdout.Close()
	defer ag.Store.dispatch(Shutdown{})
	defer ag.wg.Wait()
	defer ag.stdin.Close()
}

// NewAgent returns a new Agent, initialised using the settings in cfg.
// If cfg.Codec is invalid (see CodecNames), `DefaultCodec` will be used as the
// codec and an error returned.
// An initialised, usable agent object is always returned.
//
// For tests, NewTestingAgent(), NewTestingStore() and NewTestingCtx()
// are preferred to creating a new agent directly
func NewAgent(cfg AgentConfig) (*Agent, error) {
	var err error
	done := make(chan struct{})
	ag := &Agent{
		Name:   cfg.AgentName,
		Done:   done,
		stdin:  cfg.Stdin,
		stdout: cfg.Stdout,
		stderr: cfg.Stderr,
		handle: codecHandles[cfg.Codec],
	}
	ag.sd.done = done
	if ag.stdin == nil {
		ag.stdin = os.Stdin
	}
	if ag.stdout == nil {
		ag.stdout = os.Stdout
	}
	if ag.stderr == nil {
		ag.stderr = os.Stderr
	}
	ag.stdin = &mgutil.IOWrapper{
		Locker: &sync.Mutex{},
		Reader: ag.stdin,
		Closer: ag.stdin,
	}
	ag.stdout = &mgutil.IOWrapper{
		Locker: &sync.Mutex{},
		Writer: ag.stdout,
		Closer: ag.stdout,
	}
	ag.stderr = &mgutil.IOWrapper{
		Locker: &sync.Mutex{},
		Writer: ag.stderr,
	}
	ag.Log = NewLogger(ag.stderr)
	ag.Store = newStore(ag, ag.listener).
		Before(defaultReducers.before...).
		Use(defaultReducers.use...).
		After(defaultReducers.after...)

	if e := os.Getenv("MARGO_BUILD_ERROR"); e != "" {
		ag.Store.Use(Reduce(func(mx *Ctx) *State {
			return mx.AddStatus(e)
		}))
	}

	if ag.handle == nil {
		err = fmt.Errorf("Invalid codec '%s'. Expected %s", cfg.Codec, CodecNamesStr)
		ag.handle = codecHandles[DefaultCodec]
	}
	ag.encWr = bufio.NewWriter(ag.stdout)
	ag.enc = codec.NewEncoder(ag.encWr, ag.handle)
	ag.dec = codec.NewDecoder(bufio.NewReader(ag.stdin), ag.handle)

	return ag, err
}

// Args returns a new copy of agent's Args.
func (ag *Agent) Args() Args {
	return Args{
		Store: ag.Store,
		Log:   ag.Log,
	}
}
