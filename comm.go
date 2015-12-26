package gomsg

// the protocol is:
// PUB, PUSH, REQ, REQALL - |type(1)|sequence(4)|topic(4+n)|payload(4+n)|
// ACK, NACK, REP, ERR, REP_PARTIAL, ERR_PARTIAL - |type(1)|sequence(4)|payload(4+n)|
//
// handler success functions can be func(IContext, any)(any, error). Every argument and return type is optional
// there are three kinds of client outgoing comunications:
// * PUB - a message is sent to all destinations registered to this message and there is no reply
// * PUSH - a message is sent, in turn, to all the destinations returning on the first success. The destinations rotate.
//          There is an acknowledgement from the
// * REQ - Same as PUSH but it returns a payload
// * REQALL - The message is sent to all subscribed endpoints. Only finishes when all endpoints have replyied or a timeout has occurred.
//
// * REP - response in REQ
// * ERR - Error in a REQ
// * REP_PARTIAL - reply in a REQ_ALL
// * ERR_PARTIAL - error in a  REQ_ALL
// * ACK - acknowledges the delivery of a message by PUSH or the End Of Replies
// * NACK - failed to acknowledges the delivery of a message by PUSH (sent by the endpoint)
//
// Sending messages has no buffering. If a destination is not present to consume the message
// when it is sent, then the message will be lost.
// When a client connection is established, the client "wire" sends its topic to the other end (server)
// and receives the remote topics. After this it starts receiving/sending messages.
// Whenever the local endpoint has a new topic it sends that information to the remote endpoint.
//
// Context has ALL the information of the delivered message. It can be used as the only parameter of the handler function. ex: func (ctx IRequest)
// A reply can be sent by using ctx.SetReply([]byte), ctx.SetFault(), ctx.Writer().WriteXXX()
//
// If we send a message, if the type is different of mybus.Msg, then the message will be serialized by using the defined decoder.
// When receiving the message, if the function handler, has a parameter different than IRequest/IResponse it will deserialize the payload using the defined decoder.

import (
	"bufio"
	"bytes"
	//"crypto/rand"
	//"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	_   EKind = iota
	SUB       // indicates that wants to receive messages for a topic
	UNSUB
	REQ
	REQALL
	PUSH
	PUB
	REP         // terminates the REQALL
	ERR         // terminates the REQALL
	REP_PARTIAL // partial reply from a request all
	ERR_PARTIAL // partial error from a request all
	ACK         // Successful delivery of a PUSH message or it is a End Of Replies
	NACK
)

var kind_labels = [...]string{
	"SUB",
	"UNSUB",
	"REQ",
	"REQALL",
	"PUSH",
	"PUB",
	"REP",
	"ERR",
	"REP_PARTIAL",
	"ERR_PARTIAL",
	"ACK",
	"NACK",
}

type EKind uint8

func (this EKind) String() string {
	idx := int(this)
	if idx > 0 && idx <= len(kind_labels) {
		return kind_labels[idx-1]
	} else {
		return fmt.Sprint("unknown:", idx)
	}
}

const FILTER_TOKEN = "*"

var (
	errorType   = reflect.TypeOf((*error)(nil)).Elem()    // interface type
	replyType   = reflect.TypeOf((*Response)(nil)).Elem() // interface type
	requestType = reflect.TypeOf((**Request)(nil)).Elem() // interface type
)

// errors
var (
	DROPPED      = errors.New("Message was Droped. Wire is closed or full.")
	NOCODEC      = errors.New("No codec define")
	UNKNOWNTOPIC = errors.New("No registered subscriber")
	TIMEOUT      = errors.New("Timeout while waiting for reply")
	EOR          = errors.New("End Of Multiple Reply")
	NACKERROR    = errors.New("Not Acknowledge Error")
)

type SafeConn struct {
	conn    net.Conn
	timeout time.Duration
}

func (this SafeConn) Write(p []byte) (n int, err error) {
	if this.timeout == time.Duration(0) {
		this.conn.SetWriteDeadline(time.Time{})
	} else {
		this.conn.SetWriteDeadline(time.Now().Add(this.timeout))
	}
	return this.conn.Write(p)
}

func (this SafeConn) Read(p []byte) (n int, err error) {
	if this.timeout == time.Duration(0) {
		this.conn.SetReadDeadline(time.Time{})
	} else {
		this.conn.SetReadDeadline(time.Now().Add(this.timeout))
	}
	return this.conn.Read(p)
}

// ensure interface
var _ io.Writer = &SafeConn{}
var _ io.Reader = &SafeConn{}

/*
type IContext interface {
	Connection() net.Conn
	Kind() EKind
	Name() string
}

type IResponse interface {
	IContext

	Reader() *InputStream
	Reply() []byte
	Fault() error
	Last() bool    // is the last reply?
	EndMark() bool // is this the end mark?
}

type IRequest interface {
	IResponse

	Request() []byte
	Writer() *OutputStream
	// sets a single reply (REQ)
	SetReply([]byte)
	// sets a single reply error (ERR)
	SetFault(error)
	// Terminates a series of replies by sending an ACK message to the caller.
	// It can also be used to reject a request, since this terminates the request without sending a reply payload.
	Terminate()
	// sets a multi reply (REQ_PARTIAL)
	SendReply([]byte)
	// sets a multi reply error (ERR_PARTIAL)
	SendFault(error)
	SendAs(EKind, []byte)
}
*/

// Context is used to pass all the data to replying function if we so wish.
// payload contains the data for incoming messages and reply contains the reply data
// err is used in REPlies in case something went wrong
type Context struct {
	wire     *Wire
	conn     net.Conn
	Kind     EKind
	Name     string
	sequence uint32
}

func (this *Context) Connection() net.Conn {
	return this.conn
}

type Response struct {
	*Context

	reply []byte
	fault error
}

//var _ IResponse = Response{}

func NewResponse(wire *Wire, c net.Conn, kind EKind, seq uint32, payload []byte) Response {
	ctx := Response{
		Context: &Context{
			wire:     wire,
			conn:     c,
			Kind:     kind,
			sequence: seq,
		},
	}

	if kind == ERR || kind == ERR_PARTIAL {
		ctx.fault = errors.New(string(payload))
	}
	ctx.reply = payload

	return ctx
}

func (this Response) Reader() *InputStream {
	return NewInputStream(bytes.NewReader(this.reply))
}

func (this Response) Reply() []byte {
	return this.reply
}

func (this Response) Fault() error {
	return this.fault
}

// is the last reply?
func (this Response) Last() bool {
	return this.Kind != REP_PARTIAL && this.Kind != ERR_PARTIAL
}

// is this the end mark?
func (this Response) EndMark() bool {
	return this.Kind == ACK
}

type Request struct {
	Response

	request   []byte
	writer    *bytes.Buffer
	terminate bool
	noreply   bool
}

//var _ IRequest = &Request{}

func NewRequest(wire *Wire, c net.Conn, msg Envelope) *Request {
	ctx := &Request{
		Response: Response{
			Context: &Context{
				wire:     wire,
				conn:     c,
				Kind:     msg.kind,
				Name:     msg.name,
				sequence: msg.sequence,
			},
		},
		request: msg.payload,
	}

	return ctx
}

func (this *Request) reset() {
	this.fault = nil
	this.reply = nil
	this.writer = nil
	this.terminate = false
}

func (this *Request) Request() []byte {
	return this.request
}

func (this *Request) Writer() *OutputStream {
	this.writer = new(bytes.Buffer)
	return NewOutputStream(this.writer)
}

// sets a single reply error (ERR)
func (this *Request) SetFault(err error) {
	this.reset()
	this.fault = err
}

// sets a single reply (REQ)
func (this *Request) SetReply(payload []byte) {
	this.reset()
	this.reply = payload
}

func (this *Request) Reply() []byte {
	if this.writer != nil {
		this.reply = this.writer.Bytes()
		this.writer = nil
	}
	return this.reply
}

// Terminates a series of replies by sending an ACK message to the caller.
// It can also be used to reject a request, since this terminates the request without sending a reply payload.
func (this *Request) Terminate() {
	this.reset()
	this.terminate = true
}

// sets a multi reply (REQ_PARTIAL)
func (this *Request) SendReply(reply []byte) {
	this.SendAs(REP_PARTIAL, reply)
}

// sets a multi reply error (ERR_PARTIAL)
func (this *Request) SendFault(err error) {
	var reply []byte
	w := this.wire
	if w.codec != nil {
		reply, _ = w.codec.Encode(err.Error())
	} else {
		reply = []byte(err.Error())
	}
	this.SendAs(ERR_PARTIAL, reply)
}

func (this *Request) SendAs(kind EKind, reply []byte) {
	this.wire.reply(kind, this.sequence, reply)
}

//var _ IContext = &Context{}

type Msg struct {
	*OutputStream
	buffer *bytes.Buffer
}

func NewMsg() *Msg {
	var buffer = new(bytes.Buffer)

	return &Msg{
		NewOutputStream(buffer),
		buffer,
	}
}

type Envelope struct {
	kind     EKind
	sequence uint32
	name     string
	payload  []byte
	handler  func(ctx Response)
	timeout  time.Duration
	errch    chan error
}

func (this Envelope) String() string {
	return fmt.Sprintf("{kind:%s, sequence:%d, name:%s, payload:%s, handler:%p, timeout:%s, errch:%p",
		this.kind, this.sequence, this.name, this.payload, this.handler, this.timeout, this.errch)
}

type EnvelopeConn struct {
	message Envelope
	conn    net.Conn
}

type Wire struct {
	chin      chan Envelope
	handlerch chan EnvelopeConn

	codec    Codec
	sequence uint32
	timeout  time.Duration

	mucb      sync.RWMutex
	callbacks map[uint32]chan Response

	disconnect  func(error)
	findHandler func(name string) func(ctx *Request)

	mutop        sync.RWMutex
	groupId      string // When it is a client wire, it is used for High Availability
	remoteTopics map[string]bool
}

func NewWire(codec Codec) *Wire {
	wire := &Wire{
		callbacks:    make(map[uint32]chan Response),
		timeout:      time.Second * 20,
		codec:        codec,
		remoteTopics: make(map[string]bool),
		chin:         make(chan Envelope, 1000),
		handlerch:    make(chan EnvelopeConn, 1000),
	}

	return wire
}

func (this *Wire) Destroy() {
	if this.chin != nil {
		close(this.chin)
		close(this.handlerch)

		this.chin = nil
		this.handlerch = nil
	}
}

func (this *Wire) addRemoteTopic(name string) {
	this.mutop.Lock()
	defer this.mutop.Unlock()

	this.remoteTopics[name] = true
}

func (this *Wire) deleteRemoteTopic(name string) {
	this.mutop.Lock()
	defer this.mutop.Unlock()
	delete(this.remoteTopics, name)
}

func (this *Wire) hasRemoteTopic(name string) bool {
	this.mutop.Lock()
	defer this.mutop.Unlock()

	var prefix string
	for k, _ := range this.remoteTopics {
		if strings.HasSuffix(k, FILTER_TOKEN) {
			prefix = k[:len(k)-1]
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == k {
			return true
		}
	}
	return false
}

func (this *Wire) asynchWaitForCallback(msg Envelope) {
	this.mucb.Lock()
	defer this.mucb.Unlock()

	// frame channel
	ch := make(chan Response, 1)
	this.callbacks[msg.sequence] = ch
	// since we can have multi replies, we have to calculate the deadline for all of them
	deadline := time.Now().Add(msg.timeout)
	// error channel
	go func() {
		for {
			select {
			case <-time.After(deadline.Sub(time.Now())):
				this.delCallback(msg.sequence)
				msg.errch <- TIMEOUT
				break

			case ctx := <-ch:
				if msg.handler != nil {
					msg.handler(ctx)
					// only breaks the loop if it is the last one
					// allowing to receive multiple replies
					if ctx.Last() {
						msg.errch <- nil
						break
					}
				} else {
					if ctx.Kind == ACK {
						msg.errch <- nil
					} else if ctx.Kind == NACK {
						msg.errch <- NACKERROR
					}
					break
				}
			}
		}
	}()
}

func (this *Wire) getCallback(seq uint32, last bool) chan Response {
	this.mucb.Lock()
	defer this.mucb.Unlock()

	cb := this.callbacks[seq]
	if last {
		delete(this.callbacks, seq)
	}
	return cb
}

func (this *Wire) delCallback(seq uint32) {
	this.mucb.Lock()
	defer this.mucb.Unlock()

	delete(this.callbacks, seq)
}

func (this *Wire) SetTimeout(timeout time.Duration) {
	this.timeout = timeout
}

func (this *Wire) Send(msg Envelope) <-chan error {
	msg.errch = make(chan error, 1)
	// check first if the peer has the topic before sending.
	// This is done because remote topics are only available after a connection
	if test(msg.kind, PUB, PUSH, REQ, REQALL) && !this.hasRemoteTopic(msg.name) {
		msg.errch <- UNKNOWNTOPIC
	} else { // SUB, UNSUB
		msg.sequence = atomic.AddUint32(&this.sequence, 1)
		//msg.sequence = randomSeq()
		this.enqueue(msg)
	}
	return msg.errch
}

func (this *Wire) enqueue(msg Envelope) bool {
	ok := true
	select {
	case this.chin <- msg:
	default:
		ok = false
		msg.errch <- DROPPED
	}
	return ok
}

func (this *Wire) reply(kind EKind, seq uint32, payload []byte) {
	msg := this.NewReplyEnvelope(kind, seq, payload)
	if this.enqueue(msg) {
		err := <-msg.errch
		if err != nil {
			this.Destroy()
		}
	}
}

/*
func randomSeq() uint32 {
	b := make([]byte, 4)
	rand.Read(b)
	seq := binary.LittleEndian.Uint32(b)
	return seq
}
*/

func (this *Wire) NewReplyEnvelope(kind EKind, seq uint32, payload []byte) Envelope {
	return Envelope{
		kind:     kind,
		sequence: seq,
		payload:  payload,
		timeout:  time.Second,
		errch:    make(chan error, 1),
	}
}

func (this *Wire) writer(c net.Conn) {
	for msg := range this.chin {
		err := this.write(c, msg)
		if err != nil {
			msg.errch <- err
		} else {
			if test(msg.kind, SUB, UNSUB, REQ, REQALL, PUSH) {
				// wait the reply asynchronously
				this.asynchWaitForCallback(msg)
			} else {
				msg.errch <- nil
			}
		}
	}
	// exit
	this.disconnect(nil)
}

func (this *Wire) write(c net.Conn, msg Envelope) (err error) {
	defer func() {
		if err != nil {
			this.disconnect(err)
		}
	}()

	buf := bufio.NewWriter(SafeConn{c, msg.timeout})
	w := NewOutputStream(buf)

	// kind
	err = w.WriteUI8(uint8(msg.kind))
	if err != nil {
		return
	}
	// channel sequence
	err = w.WriteUI32(msg.sequence)
	if err != nil {
		return
	}

	if test(msg.kind, PUB, PUSH, SUB, UNSUB, REQ, REQALL) {
		// topic
		err = w.WriteString(msg.name)
		if err != nil {
			return
		}
	}
	if test(msg.kind, REQ, REQALL, PUB, PUSH, REP, REP_PARTIAL, ERR, ERR_PARTIAL) {
		// payload
		err = w.WriteBytes(msg.payload)
		if err != nil {
			return
		}
	}

	return buf.Flush()
}

func read(r *InputStream, bname bool, bpayload bool) (name string, payload []byte, err error) {
	if bname {
		name, err = r.ReadString()
		if err != nil {
			return
		}
	}
	if bpayload {
		payload, err = r.ReadBytes()
		if err != nil {
			return
		}
	}
	return
}

func (this *Wire) reader(c net.Conn) {
	//r := NewInputStream(ConnTimeout{c, this.timeout})
	r := NewInputStream(c)
	var err error
	for {
		// kind
		var k uint8
		k, err = r.ReadUI8()
		if err != nil {
			break
		}
		var kind EKind = EKind(k)

		// sequence
		var seq uint32
		seq, err = r.ReadUI32()
		if err != nil {
			break
		}

		var name string
		var data []byte

		if test(kind, SUB, UNSUB, REQ, REQALL, PUSH, PUB) {
			if kind == SUB || kind == UNSUB {
				// un/register topic from peer
				name, _, err = read(r, true, false)
				if err != nil {
					break
				}
				if kind == SUB {
					this.addRemoteTopic(name)
				} else {
					this.deleteRemoteTopic(name)
				}
				msg := this.NewReplyEnvelope(ACK, seq, nil)
				if this.enqueue(msg) {
					err = <-msg.errch
					if err != nil {
						break
					}
				}

			} else {
				name, data, err = read(r, true, true)
				if err != nil {
					break
				}

				this.handlerch <- EnvelopeConn{
					message: Envelope{
						kind:     kind,
						sequence: seq,
						name:     name,
						payload:  data,
					},
					conn: c,
				}
			}
		} else {
			last := true
			switch kind {
			case REP_PARTIAL, REP:
				_, data, err = read(r, false, true)
				if err != nil {
					break
				}
				last = kind == REP

			case ERR, ERR_PARTIAL:
				_, data, err = read(r, false, true)
				if err != nil {
					break
				}
				// reply was an error
				var s string
				if this.codec != nil {
					this.codec.Decode(data, &s)
					data = []byte(s)
				}
				last = kind == ERR
			}

			cb := this.getCallback(seq, last)
			if cb != nil {
				cb <- NewResponse(this, c, kind, seq, data)
			} else {
				fmt.Printf("< No callback found for kind=%s, sequence=%d.\n", kind, seq)
			}
		}
	}
	/*
		if err == io.EOF {
			fmt.Println("< client", c.RemoteAddr(), "closed connection")
		} else if err != nil {
			fmt.Println("< error:", err)
		}
	*/

	this.disconnect(err)
}

// Executes the function that handles the request. By default the reply of this handler is allways final (REQ or ERR).
// If we wish  to send multiple replies, we must cancel the response and then send the several replies.
func (this *Wire) runHandler() {
	for msgconn := range this.handlerch {
		msg := msgconn.message
		c := msgconn.conn
		if msg.kind == REQ || msg.kind == REQALL {
			// Serve
			var reply []byte
			var err error
			handler := this.findHandler(msg.name)
			var noreply bool
			var terminate bool
			if handler != nil {
				ctx := NewRequest(this, c, msg)
				handler(ctx) // this resets payload
				terminate = ctx.terminate
				noreply = ctx.noreply
				if ctx.Fault() != nil {
					err = ctx.Fault() // single fault
				} else {
					reply = ctx.Reply() // single reply
				}
			} else {
				err = UNKNOWNTOPIC
			}

			// only sends a reply if there was some kind of return action
			if terminate {
				this.reply(ACK, msg.sequence, nil)
			} else if !noreply {
				if err != nil {
					msg.kind = ERR
					if this.codec != nil {
						reply, _ = this.codec.Encode(err.Error())
					} else {
						reply = []byte(err.Error())
					}
				} else {
					msg.kind = REP
				}
				this.reply(msg.kind, msg.sequence, reply)
			}
		} else { // PUSH & PUB
			handler := this.findHandler(msg.name)

			if msg.kind == PUSH {
				if handler != nil {
					msg.kind = ACK
				} else {
					msg.kind = NACK
				}
				this.reply(msg.kind, msg.sequence, nil)
			}

			if handler != nil {
				handler(NewRequest(this, c, msg))
			}
		}
	}
}

func validateSigErr(params []reflect.Type, contextType reflect.Type) (bool, bool, reflect.Type, bool) {
	order := 0
	var hasContext, hasError bool
	var payloadType reflect.Type
	for _, v := range params {
		if v == contextType {
			if order >= 1 {
				return false, false, nil, false
			}
			hasContext = true
			order = 1
		} else if v == errorType {
			if order >= 3 {
				return false, false, nil, false
			}
			hasError = true
			order = 3
		} else {
			if order >= 2 {
				return false, false, nil, false
			}
			payloadType = v
			order = 2
		}
	}
	return true, hasContext, payloadType, hasError
}

func createReplyHandler(codec Codec, fun interface{}) func(ctx Response) {
	// validate input
	var payloadType reflect.Type
	var hasContext, hasError, ok bool
	function := reflect.ValueOf(fun)
	typ := function.Type()

	// validate argument types
	size := typ.NumIn()
	if size > 3 {
		panic("Invalid function. Function can only have at the most 3 parameters.")
	} else {
		params := make([]reflect.Type, 0, size)
		for i := 0; i < size; i++ {
			params = append(params, typ.In(i))
		}
		ok, hasContext, payloadType, hasError = validateSigErr(params, replyType)
		if !ok {
			panic("Invalid function. All parameter function are optional but they must folow the signature order: func([mybus.IReply], [custom type], [error]).")
		}
	}

	return func(ctx Response) {
		var p reflect.Value
		params := make([]reflect.Value, 0)
		if hasContext {
			params = append(params, reflect.ValueOf(ctx))
		}
		if payloadType != nil {
			if codec != nil && ctx.reply != nil {
				p = reflect.New(payloadType)
				codec.Decode(ctx.reply, p.Interface())
				params = append(params, p.Elem())
			} else {
				// when the codec is nil the data is sent as is
				if ctx.reply == nil {
					params = append(params, reflect.Zero(payloadType))
				} else {
					params = append(params, reflect.ValueOf(ctx.reply))
				}
			}
		}
		if hasError {
			var err error = ctx.Fault()
			params = append(params, reflect.ValueOf(&err).Elem())
		}

		function.Call(params)
	}

}

func createRequestHandler(codec Codec, fun interface{}) func(ctx *Request) {
	// validate input
	var payloadType reflect.Type
	function := reflect.ValueOf(fun)
	typ := function.Type()
	// validate argument types
	size := typ.NumIn()
	if size > 2 {
		panic("Invalid function. Function can only have at the most two  parameters.")
	} else if size > 1 {
		t := typ.In(0)
		if t != requestType {
			panic("Invalid function. In a two paramater function the first must be the mybus.IRequest.")
		}
	}

	var hasContext bool
	if size == 2 {
		payloadType = typ.In(1)
		hasContext = true
	} else if size == 1 {
		t := typ.In(0)
		if t != requestType {
			payloadType = t
		} else {
			hasContext = true
		}
	}
	// validate return
	size = typ.NumOut()
	if size > 2 {
		panic("functions can only have at the most two return values.")
	} else if size > 1 && errorType != typ.Out(1) {
		panic("In a two return values functions the second value must be an error.")
	}

	return func(ctx *Request) {
		var p reflect.Value
		params := make([]reflect.Value, 0)
		if hasContext {
			params = append(params, reflect.ValueOf(ctx))
		}
		if payloadType != nil {
			if codec != nil && ctx.request != nil {
				p = reflect.New(payloadType)
				codec.Decode(ctx.request, p.Interface())
				params = append(params, p.Elem())
			} else {
				// when the codec is nil the data is sent as is
				if ctx.request == nil {
					params = append(params, reflect.Zero(payloadType))
				} else {
					params = append(params, reflect.ValueOf(ctx.request))
				}
			}
		}

		results := function.Call(params)

		// check for error
		var result []byte
		var err error
		if len(results) > 0 {
			for k, v := range results {
				if v.Type() == errorType {
					if !v.IsNil() {
						err = v.Interface().(error)
					}
					break
				} else {
					// stores the result to return at the end of the check
					data := results[k].Interface()
					if codec != nil {
						result, err = codec.Encode(data)
						if err != nil {
							break
						}
					} else {
						// when the codec is nil the data is sent as is
						result = data.([]byte)
					}
				}
			}

			if err != nil {
				ctx.SetFault(err)
			} else {
				ctx.SetReply(result)
			}
		} else {
			ctx.noreply = true
		}
	}
}

type ClientServer struct {
	codec    Codec
	muhnd    sync.RWMutex
	handlers map[string]func(ctx *Request)

	timeout   time.Duration
	muconn    sync.RWMutex
	OnConnect func(w *Wired)
	OnClose   func(c net.Conn)
}

// timeout used to send data
func (this *ClientServer) SetTimeout(timeout time.Duration) {
	this.timeout = timeout
}

// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *ClientServer) createEnvelope(kind EKind, name string, payload interface{}, success interface{}, timeout time.Duration) (Envelope, error) {
	var handler func(ctx Response)
	if success != nil {
		handler = createReplyHandler(this.codec, success)
	}

	msg := Envelope{
		kind:    kind,
		name:    name,
		handler: handler,
		timeout: timeout,
	}

	if payload != nil {
		switch m := payload.(type) {
		case []byte:
			msg.payload = m
		case *Msg:
			msg.payload = m.buffer.Bytes()
		default:
			var err error
			msg.payload, err = this.codec.Encode(payload)
			if err != nil {
				return Envelope{}, err
			}
		}
	}

	return msg, nil
}

type Client struct {
	ClientServer

	addr                 string
	wire                 *Wire
	conn                 net.Conn
	reconnectInterval    time.Duration
	reconnectMaxInterval time.Duration
}

func NewClient() *Client {
	codec := JsonCodec{}
	this := &Client{
		wire:                 NewWire(codec),
		reconnectInterval:    time.Millisecond * 100,
		reconnectMaxInterval: 0,
	}
	this.codec = codec
	this.timeout = time.Second * 20
	this.handlers = make(map[string]func(ctx *Request))

	this.wire.findHandler = func(name string) func(ctx *Request) {
		this.muhnd.RLock()
		defer this.muhnd.RUnlock()

		return findHandler(name, this.handlers)
	}

	this.wire.disconnect = func(e error) {
		//this.Disconnect()
		this.disconnect()
	}
	return this
}

func (this *Client) SetReconnectInterval(reconnectInterval time.Duration) *Client {
	this.reconnectInterval = reconnectInterval
	return this
}

func (this *Client) SetReconnectMaxInterval(reconnectMaxInterval time.Duration) *Client {
	this.reconnectMaxInterval = reconnectMaxInterval
	return this
}

func (this *Client) SetCodec(codec Codec) *Client {
	this.wire.codec = codec
	this.codec = codec
	return this
}

// Make it belong to a group.
// Only one element at a time (round-robin) handles the messages.
func (this *Client) SetGroupId(groupId string) *Client {
	this.wire.groupId = groupId
	return this
}

func (this *Client) handshake(c net.Conn) error {
	this.muhnd.Lock()
	size := len(this.handlers) + 1
	payload := make([]string, size)
	payload[0] = this.wire.groupId
	i := 1
	for k, _ := range this.handlers {
		payload[i] = k
		i++
	}
	this.muhnd.Unlock()

	err := serializeHanshake(c, this.codec, this.timeout, payload)
	if err != nil {
		return err
	}

	// get reply
	_, remoteTopics, err := deserializeHandshake(c, this.codec, this.timeout, false)
	if err != nil {
		return err
	}
	this.wire.mutop.Lock()
	this.wire.remoteTopics = remoteTopics
	this.wire.mutop.Unlock()

	return nil
}

func serializeHanshake(c net.Conn, codec Codec, timeout time.Duration, payload []string) error {
	var data []byte
	var err error
	if codec != nil {
		data, err = codec.Encode(payload)
		if err != nil {
			return err
		}
	} else {
		var buf = new(bytes.Buffer)
		os := NewOutputStream(buf)
		os.WriteUI16(uint16(len(payload)))
		if err != nil {
			return err
		}
		for _, v := range payload {
			os.WriteString(v)
		}
		data = buf.Bytes()
	}

	buf := bufio.NewWriter(SafeConn{c, timeout})
	w := NewOutputStream(buf)
	err = w.WriteBytes(data)
	if err != nil {
		return err
	}
	return buf.Flush()
}

func deserializeHandshake(c net.Conn, codec Codec, timeout time.Duration, isFromClient bool) (string, map[string]bool, error) {
	r := NewInputStream(SafeConn{c, timeout})
	data, err := r.ReadBytes()
	if err != nil {
		return "", nil, err
	}
	var topics []string
	if codec != nil {
		codec.Decode(data, &topics)
	} else {
		var buf = bytes.NewBuffer(data)
		is := NewInputStream(buf)
		size, err := is.ReadUI16()
		if err != nil {
			return "", nil, err
		}
		topics = make([]string, size)
		var length int = int(size)
		for i := 0; i < length; i++ {
			topics[i], err = is.ReadString()
			if err != nil {
				return "", nil, err
			}
		}
	}
	var identity string
	if isFromClient {
		identity = topics[0]
		topics = topics[1:]
	}
	remoteTopics := make(map[string]bool)
	for _, v := range topics {
		remoteTopics[v] = true
	}
	return identity, remoteTopics, nil
}

// connect is seperated to allow the definition and use of OnConnect
func (this *Client) Connect(addr string) *Client {
	this.addr = addr
	this.dial(this.reconnectInterval)
	return this
}

func (this *Client) Reconnect() {
	this.disconnect()
}

func (this *Client) Disconnect() {
	this.muconn.Lock()
	defer this.muconn.Unlock()

	if this.conn != nil {
		this.conn.Close()
		if this.OnClose != nil {
			this.OnClose(this.conn)
		}
		this.conn = nil
	}
}

func (this *Client) disconnect() {
	this.muconn.Lock()
	defer this.muconn.Unlock()

	if this.conn != nil {
		this.conn.Close()
		if this.OnClose != nil {
			this.OnClose(this.conn)
		}
		this.conn = nil
		if this.reconnectInterval > 0 {
			go func(interval time.Duration) {
				this.dial(interval)
			}(this.reconnectInterval)
		}
	}
}

func (this *Client) Destroy() {
	this.reconnectInterval = 0
	this.wire.Destroy()
	this.Disconnect()
}

// tries do dial. If dial fails and if reconnectInterval > 0
// it schedules a new try within reconnectInterval milliseconds
func (this *Client) dial(retry time.Duration) {
	this.muconn.Lock()
	defer this.muconn.Unlock()

	// gets the connection
	c, err := net.DialTimeout("tcp", this.addr, time.Second)
	if err != nil {
		fmt.Println("> I: failed to connect to", this.addr)
		if retry > 0 {
			fmt.Println("> I: retry in", retry)
			go func() {
				time.Sleep(retry)
				if this.reconnectMaxInterval > 0 && retry < this.reconnectMaxInterval {
					retry = retry * 2
				}
				this.dial(retry)
			}()
		} else {
			fmt.Println("> I: NO retry will be performed!")
		}
		return
	}

	fmt.Println("> I: connected to", this.addr, "with", c.LocalAddr())
	// topic exchange
	err = this.handshake(c)
	if err != nil {
		fmt.Println("> E: error:", err)
		c.Close()
	} else {
		go this.wire.writer(c)
		go this.wire.reader(c)
		go this.wire.runHandler()

		this.conn = c

		if this.OnConnect != nil {
			this.OnConnect(&Wired{c, this.wire})
		}
	}
}

func (this *Client) Connection() net.Conn {
	return this.conn
}

// name can have an '*' at the end, meaning that it will handle messages
// with the destiny name starting with the reply name whitout the '*'.
// When handling request messages, the function handler can have a return value and/or an error.
// When handling publish/push messages, any return from the function handler is discarded.
func (this *Client) Handle(name string, fun interface{}) {
	handler := createRequestHandler(this.wire.codec, fun)

	this.muhnd.Lock()
	this.handlers[name] = handler
	this.muhnd.Unlock()

	this.muconn.Lock()
	defer this.muconn.Unlock()

	if this.conn != nil {
		msg, _ := this.createEnvelope(SUB, name, nil, nil, this.timeout)
		this.wire.Send(msg)
	}
}

func (this *Client) Cancel(name string) {
	this.muhnd.Lock()
	delete(this.handlers, name)
	this.muhnd.Unlock()

	this.muconn.Lock()
	defer this.muconn.Unlock()

	if this.conn != nil {
		msg, _ := this.createEnvelope(UNSUB, name, nil, nil, this.timeout)
		this.wire.Send(msg)
	}
}

// sends a message and waits for the reply
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) Request(name string, payload interface{}, handler interface{}) <-chan error {
	return this.RequestTimeout(name, payload, handler, this.timeout)
}

// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) RequestTimeout(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQ, name, payload, handler, timeout)
}

// Requests messages to all connected clients of the same server. If a client is not connected it is forever lost.
func (this *Client) RequestAll(name string, payload interface{}, handler interface{}) <-chan error {
	return this.Send(REQALL, name, payload, handler, this.timeout)
}

// Requests messages to all connected clients of the same server. If a client is not connected it is forever lost.
func (this *Client) RequestAllTimeout(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQALL, name, payload, handler, timeout)
}

// sends a message and receive an acknowledge
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) Push(name string, m interface{}) <-chan error {
	return this.PushTimeout(name, m, this.timeout)
}

// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) PushTimeout(name string, m interface{}, timeout time.Duration) <-chan error {
	return this.Send(PUSH, name, m, nil, timeout)
}

// sends a message without any reply
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) Publish(name string, m interface{}) <-chan error {
	return this.PublishTimeout(name, m, this.timeout)
}

// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) PublishTimeout(name string, m interface{}, timeout time.Duration) <-chan error {
	return this.Send(PUB, name, m, nil, timeout)
}

// When the payload is of type []byte it passes the raw bytes without encoding.
// When the payload is of type mybus.Msg it passes the FRAMED raw bytes without encoding.
func (this *Client) Send(kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	msg, err := this.createEnvelope(kind, name, payload, handler, timeout)
	if err != nil {
		ch := make(chan error, 1)
		ch <- err
		return ch
	}
	return this.wire.Send(msg)
}

type Wired struct {
	conn net.Conn
	wire *Wire
}

func (this *Wired) Conn() net.Conn {
	return this.conn
}

func (this *Wired) Wire() *Wire {
	return this.wire
}

// Connections are grouped accordingly to its group id.
// A wire with an empty group id means all nodes are different.
//
// Wires with the same non empty id are treated as mirrors of each other.
// This means that we only need to call one of them.
// The other nodes function as High Availability and load balancing nodes
type Wires struct {
	mu     sync.RWMutex
	wires  []*Wired
	groups map[string][]*Wired
	cursor int
}

func NewWires() *Wires {
	return &Wires{
		wires:  make([]*Wired, 0),
		groups: make(map[string][]*Wired),
	}
}

func (this *Wires) Destroy() {
	this.mu.Lock()
	defer this.mu.Unlock()

	for _, v := range this.wires {
		v.wire.Destroy()
		v.conn.Close()
		v.conn = nil
	}
	this.wires = make([]*Wired, 0)
	this.groups = make(map[string][]*Wired)
}

func (this *Wires) Put(conn net.Conn, wire *Wire) *Wired {
	this.mu.Lock()
	defer this.mu.Unlock()

	for _, v := range this.wires {
		if v.conn == conn {
			v.wire = wire
			return v
		}
	}
	wired := &Wired{conn, wire}
	this.wires = append(this.wires, wired)
	group := this.groups[wire.groupId]
	if group == nil {
		group = make([]*Wired, 0)
	}
	this.groups[wire.groupId] = append(group, wired)
	return wired
}

func (this *Wires) Get(conn net.Conn) *Wired {
	this.mu.Lock()
	defer this.mu.Unlock()

	for _, v := range this.wires {
		if v.conn == conn {
			return v
		}
	}
	return nil
}

func (this *Wires) Kill(conn net.Conn) {
	this.mu.Lock()
	defer this.mu.Unlock()

	var w *Wired
	this.wires, w = remove(conn, this.wires)
	if w != nil {
		group := this.groups[w.wire.groupId]
		if group != nil {
			this.groups[w.wire.groupId], _ = remove(conn, group)
		}
		w.wire.Destroy()
		w.conn.Close()
		w.conn = nil
	}

}

func remove(conn net.Conn, wires []*Wired) ([]*Wired, *Wired) {
	for k, v := range wires {
		if v.conn == conn {
			return append(wires[:k], wires[k+1:]...), v
		}
	}
	return wires, nil
}

func (this *Wires) GetAll() ([]*Wired, int) {
	return this.list(false)
}

func (this *Wires) Rotate() ([]*Wired, int) {
	return this.list(true)
}

func (this *Wires) list(rotate bool) ([]*Wired, int) {
	this.mu.RLock()
	w := make([]*Wired, len(this.wires))
	copy(w, this.wires)
	this.mu.RUnlock()

	this.mu.Lock()
	size := len(this.wires)
	if this.cursor >= size {
		this.cursor = 0
	}

	c := this.cursor

	if rotate {
		this.cursor++
	}
	this.mu.Unlock()

	return w, c

	/*
		r := make([]*Wired, size)
		for i, v := range this.wires {
			r[i] = v
		}
		// rotate
		if rotate && size > 1 {
			this.wires = append(this.wires[1:], this.wires[:1]...)
		}
		return r
	*/
}

func (this *Wires) RotateGroups() (map[string][]*Wired, int) {
	this.mu.RLock()
	newMap := make(map[string][]*Wired)
	for k, v := range this.groups {
		w := make([]*Wired, len(v))
		copy(w, v)
		newMap[k] = w
	}
	this.mu.RUnlock()

	this.mu.Lock()
	if this.cursor >= len(this.wires) {
		this.cursor = 0
	}

	c := this.cursor
	this.cursor++
	this.mu.Unlock()

	return newMap, c
}

type Server struct {
	ClientServer

	listener net.Listener
	Wires    *Wires
}

func NewServer() *Server {
	server := new(Server)
	server.Wires = NewWires()
	server.codec = JsonCodec{}
	server.timeout = time.Second * 20
	server.handlers = make(map[string]func(ctx *Request))
	return server
}

func (this *Server) SetCodec(codec Codec) *Server {
	this.codec = codec
	return this
}

func (this *Server) Listen(service string) error {
	// listen all interfaces
	l, err := net.Listen("tcp", service)
	if err != nil {
		return err
	}
	this.listener = l
	fmt.Println("< listening at", l.Addr())
	go func() {
		for {
			// notice that c is changed in the disconnect function
			c, err := l.Accept()
			if err != nil {
				// happens when the listener is closed
				//fmt.Println("< I: accepting no more due to error:", err)
				fmt.Println("< I: Stoped listening at", l.Addr())
				return
			}
			fmt.Printf("< I: accepted connection from %s\n", c.RemoteAddr())

			wire := NewWire(this.codec)
			wire.findHandler = this.findHandler

			// topic exchange
			group, remoteTopics, err := this.handshake(c, this.codec)

			if err != nil {
				c.Close()
			} else {
				wire.groupId = group
				wire.remoteTopics = remoteTopics
				wire.disconnect = func(e error) {
					this.muconn.Lock()
					defer this.muconn.Unlock()

					if c != nil {
						// handle errors during a connection
						if e == io.EOF {
							fmt.Println("< I: client", c.RemoteAddr(), "closed connection")
						} else if e != nil {
							fmt.Println("< E: error:", e)
						}

						this.Wires.Kill(c)
						if this.OnClose != nil {
							this.OnClose(c)
						}
						c = nil
					}
				}

				w := this.Wires.Put(c, wire)

				go wire.writer(c)
				go wire.reader(c)
				go wire.runHandler()

				if this.OnConnect != nil {
					this.OnConnect(w)
				}
			}
		}
	}()

	return nil
}

func (this *Server) Port() int {
	return this.listener.Addr().(*net.TCPAddr).Port
}

func (this *Server) handshake(c net.Conn, codec Codec) (string, map[string]bool, error) {
	// get remote topics
	group, payload, err := deserializeHandshake(c, codec, this.timeout, true)
	if err != nil {
		return "", nil, err
	}

	// send local topics
	// topic exchange
	this.muhnd.RLock()
	size := len(this.handlers)
	topics := make([]string, size)
	i := 0
	for k, _ := range this.handlers {
		topics[i] = k
		i++
	}
	this.muhnd.RUnlock()

	err = serializeHanshake(c, codec, this.timeout, topics)
	if err != nil {
		return "", nil, err
	}

	return group, payload, nil
}

func (this *Server) findHandler(name string) func(c *Request) {
	this.muhnd.RLock()
	defer this.muhnd.RUnlock()

	return findHandler(name, this.handlers)
}

// find the first match
func findHandler(name string, handlers map[string]func(ctx *Request)) func(c *Request) {
	var prefix string
	for k, v := range handlers {
		if strings.HasSuffix(k, FILTER_TOKEN) {
			prefix = k[:len(k)-1]
			if strings.HasPrefix(name, prefix) {
				return v
			}
		} else if name == k {
			return v
		}
	}
	return nil
}

func (this *Server) Destroy() {
	this.Wires.Destroy()
	this.listener.Close()
}

// sends a message and waits for the reply
// If the type of the payload is *mybus.Msg it will ignore encoding and use the internal bytes as the payload. This is useful if we want to implement a broker.
func (this *Server) Request(name string, payload interface{}, handler interface{}) <-chan error {
	return this.Send(REQ, name, payload, handler, this.timeout)
}

// Requests messages to all connected clients. If a client is not connected it is forever lost.
func (this *Server) RequestAll(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQALL, name, payload, handler, timeout)
}

// sends a message and receive an acknowledge
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Server) Push(name string, payload interface{}) <-chan error {
	return this.Send(PUSH, name, payload, nil, time.Second)
}

// sends a message without any reply
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Server) Publish(name string, payload interface{}) <-chan error {
	return this.Send(PUB, name, payload, nil, time.Second)
}

// When the payload is of type []byte it passes the raw bytes without encoding.
// When the payload is of type mybus.Msg it passes the FRAMED raw bytes without encoding.
func (this *Server) Send(kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.send(nil, kind, name, payload, handler, timeout)
}

func (this *Server) send(wire *Wire, kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	msg, err := this.createEnvelope(kind, name, payload, handler, timeout)
	errch := make(chan error, 1)
	if err != nil {
		errch <- err
		return errch
	}

	err = UNKNOWNTOPIC
	if msg.kind == PUSH || msg.kind == REQ {
		go func() {
			ws, cursor := this.Wires.Rotate()
			l := NewLooper(cursor, len(ws))
			for l.HasNext() {
				w := ws[l.Next()]
				// does not send to self
				if wire == nil || wire != w.wire {
					// REQ can also receive multiple messages from ONE replier
					err = <-w.wire.Send(msg)
					// exit on success
					if err == nil {
						break
					}
				}
			}
			errch <- err
		}()
	} else if msg.kind == PUB {
		// collects wires into groups. They will be called last.
		groups, cursor := this.Wires.RotateGroups()
		go func() {
			for id, group := range groups {
				if id == "" {
					l := NewLooper(cursor, len(group))
					for l.HasNext() {
						w := group[l.Next()]
						// do not send to self
						if wire == nil || wire != w.wire {
							e := <-w.wire.Send(msg)
							if e == nil {
								err = nil
							}
						}
					}
				} else {
					go func(ws []*Wired) {
						size := len(ws)
						l := NewLooper(cursor%size, size)
						for l.HasNext() {
							w := ws[l.Next()]
							// do not send to self
							if wire == nil || wire != w.wire {
								e := <-w.wire.Send(msg)
								// send only to one.
								// stop if there was a success.
								if e == nil {
									err = nil
									break
								}
							}
						}
					}(group)
				}
			}
			errch <- err
		}()
	} else if msg.kind == REQALL {
		var wg sync.WaitGroup
		// we wrap the reply handler so that we can control the number o replies delivered.
		handler := msg.handler
		msg.handler = func(ctx Response) {
			// since the end mark will be passed to the caller when ALL replies from ALL repliers arrive,
			// the handler must be called only if it is not a end marker.
			// It is also necessary to adjust the kind so that a last kind (REQ, ERR) is not passed.
			if !ctx.EndMark() {
				kind := ctx.Kind
				if ctx.Kind == REP {
					ctx.Kind = REP_PARTIAL
				} else if ctx.Kind == ERR {
					ctx.Kind = ERR_PARTIAL
				}
				handler(ctx)
				// resets the kind
				ctx.Kind = kind
			}
		}

		groups, cursor := this.Wires.RotateGroups()
		for id, group := range groups {
			if id == "" {
				l := NewLooper(cursor, len(group))
				for l.HasNext() {
					w := group[l.Next()]
					// do not send to self
					if wire == nil || wire != w.wire {
						// increment reply counter
						wg.Add(1)
						ch := w.wire.Send(msg)
						go func() {
							// waits for the request completion
							e := <-ch
							if e == nil {
								err = nil // at least one got through
							} else {
								fmt.Println("< I:", e)
							}
							wg.Done()
						}()
					}
				}
			} else {
				// increment reply counter
				wg.Add(1)
				l := NewLooper(cursor, len(group))
				go func() {
					for l.HasNext() {
						w := group[l.Next()]
						// do not send to self
						if wire == nil || wire != w.wire {
							// waits for the request completion
							e := <-w.wire.Send(msg)
							if e == nil {
								err = nil // at least one got through
								wg.Done()
								return
							}
						}
					}
					// if it got here none was successful
					wg.Done()
				}()
			}
		}
		go func() {
			// Wait for all requests to complete.
			wg.Wait()
			fmt.Println("< D: all requests finnished")
			// pass the end mark
			handler(NewResponse(wire, nil, ACK, 0, nil))
			errch <- err
		}()
	}

	return errch
}

// name can have an '*' at the end, meaning that it will handle messages
// with the destiny name starting with the reply name (whitout the '*'9.
// When handling request messages, the function handler can have a return value and/or an error.
// When handling publish/push messages, any return from the function handler is discarded.
func (this *Server) Handle(name string, fun interface{}) {
	handler := createRequestHandler(this.codec, fun)

	this.muhnd.Lock()
	this.handlers[name] = handler
	this.muhnd.Unlock()

	msg, _ := this.createEnvelope(SUB, name, nil, nil, this.timeout)
	ws, cursor := this.Wires.GetAll()
	l := NewLooper(cursor, len(ws))
	for l.HasNext() {
		w := ws[l.Next()]
		w.wire.Send(msg)
	}
}

// Messages are from one client and delivered to another client.
// The sender client does not receive his message.
// The handler execution is canceled. Arriving replies from endpoints are piped to the requesting wire
func (this *Server) Route(name string, timeout time.Duration, before func(x *Request) bool, after func(x *Response)) {
	// This handle is called by the wire.
	// Since this handler has no declared return type, upon return no data will be sent to the caller.
	// Eventually the reply must be sent or a time will occur.
	this.Handle(name, func(r *Request) {
		// decide if it should continue (and do other thinhs like store the request data).
		if before != nil && !before(r) {
			return
		}

		wire := r.wire
		this.send(wire, r.Kind, r.Name, r.Request(), func(resp Response) {
			if after != nil {
				// do something (ex: store the response data)
				after(&resp)
			}

			wire.reply(resp.Kind, r.sequence, resp.Reply()) // sending data to the caller
		}, timeout)
	})
}

func (this *Server) Cancel(name string) {
	this.muhnd.Lock()
	delete(this.handlers, name)
	this.muhnd.Unlock()

	msg, _ := this.createEnvelope(UNSUB, name, nil, nil, this.timeout)
	ws, cursor := this.Wires.GetAll()
	l := NewLooper(cursor, len(ws))
	for l.HasNext() {
		w := ws[l.Next()]
		w.wire.Send(msg)
	}
}

/*
func isTimeout(err error) bool {
	e, ok := err.(net.Error)
	return ok && e.Timeout()
}
*/

func test(t EKind, values ...EKind) bool {
	for _, v := range values {
		if v == t {
			return true
		}
	}
	return false
}

type Duplexer interface {
	Handle(name string, fun interface{})
	Send(kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error
}

var _ Duplexer = &Client{}
var _ Duplexer = &Server{}

// Route messages between to different binding ports.
func Route(name string, src Duplexer, dest Duplexer, relayTimeout time.Duration, before func(c *Request) bool, after func(c *Response)) {
	src.Handle(name, func(req *Request) {
		if before != nil && !before(req) {
			return
		}

		dest.Send(req.Kind, req.Name, req.Request(), func(resp Response) {
			if after != nil {
				after(&resp)
			}

			req.SendAs(resp.Kind, resp.Reply())
		}, relayTimeout)
	})

}