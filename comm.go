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
// * NACK - failed to acknowledge the delivery of a message by PUSH (sent by the endpoint)
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
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quintans/toolkit/faults"
	"github.com/quintans/toolkit/log"
)

var logger = log.LoggerFor("github.com/quintans/gomsg")

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
	}

	return fmt.Sprint("unknown:", idx)
}

const FILTER_TOKEN = "*"

var (
	errorType   = reflect.TypeOf((*error)(nil)).Elem()    // interface type
	replyType   = reflect.TypeOf((*Response)(nil)).Elem() // interface type
	requestType = reflect.TypeOf((**Request)(nil)).Elem() // interface type
)

// errors
var (
	DROPPED   = errors.New("Message was Droped. Wire is closed or full.")
	NOCODEC   = errors.New("No codec defined")
	EOR       = errors.New("End Of Multiple Reply")
	NACKERROR = errors.New("Not Acknowledge Error")

	UNKNOWNTOPIC = "No registered subscriber for %s."
	TIMEOUT      = "Timeout (%s) while waiting for reply of call #%d %s(%s)=%s"
)

// Specific error types are define so that we can use type assertion, if needed
type UnknownTopic error
type TimeoutError error

func timeouttime(timeout time.Duration) time.Time {
	if timeout == time.Duration(0) {
		return time.Time{}
	} else {
		return time.Now().Add(timeout)
	}
}

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

// Connection getter
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
	r := Response{
		Context: &Context{
			wire:     wire,
			conn:     c,
			Kind:     kind,
			sequence: seq,
		},
	}

	if kind == ERR || kind == ERR_PARTIAL {
		r.fault = errors.New(string(payload))
	}
	r.reply = payload

	return r
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

	payload   []byte
	writer    *bytes.Buffer
	terminate bool

	// If an handler doesn't define a return type (error, whatever) and define an input of
	// type gomsg.Request the reply will not be sent until we call gomsg.Request.ReplyAs().
	// This is used to route messages (gomsg.Route) from server to server
	deferReply bool
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
		payload: msg.payload,
	}

	return ctx
}

func (this *Request) reset() {
	this.fault = nil
	this.reply = nil
	this.writer = nil
	this.terminate = false
}

func (this *Request) Payload() []byte {
	return this.payload
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

// DeferReply indicates that the reply won't be sent immediatly.
// The reply will eventually be sent by calling with gomsg.Request.ReplyAs().
func (this *Request) DeferReply() {
	this.deferReply = true
}

// Terminate terminates a series of replies by sending an ACK message to the caller.
// It can also be used to reject a request, since this terminates the request without sending a reply payload.
func (this *Request) Terminate() {
	this.reset()
	this.terminate = true
}

// sets a multi reply (REQ_PARTIAL)
func (this *Request) SendReply(reply []byte) {
	this.ReplyAs(REP_PARTIAL, reply)
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
	this.ReplyAs(ERR_PARTIAL, reply)
}

func (this *Request) ReplyAs(kind EKind, reply []byte) {
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
	callbacks map[uint32]chan *Response

	disconnected func(net.Conn, error)
	findHandlers func(name string) []func(ctx *Request)

	mutop         sync.RWMutex
	remoteGroupID string // it is used for High Availability
	localGroupID  string // it is used for High Availability
	remoteTopics  map[string]bool
}

func NewWire(codec Codec) *Wire {
	wire := &Wire{
		callbacks:    make(map[uint32]chan *Response),
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

	for k := range this.remoteTopics {
		if strings.HasSuffix(k, FILTER_TOKEN) {
			if strings.HasPrefix(name, k[:len(k)-1]) {
				return true
			}
		} else if name == k {
			return true
		}
	}
	return false
}

func (this *Wire) asynchWaitForCallback(msg Envelope) chan *Response {
	this.mucb.Lock()
	defer this.mucb.Unlock()

	// frame channel
	ch := make(chan *Response, 1)
	this.callbacks[msg.sequence] = ch
	// error channel
	go func() {
		for {
			select {
			case <-time.After(msg.timeout):
				this.delCallback(msg.sequence)
				msg.errch <- TimeoutError(faults.New(TIMEOUT, msg.timeout, msg.sequence, msg.kind, msg.name, msg.payload))
				return

			case r := <-ch:
				// if it is nil, it was canceled
				if r == nil {
					return
				}

				if msg.handler != nil {
					msg.handler(*r)
					// only breaks the loop if it is the last one
					// allowing to receive multiple replies
					if r.Last() {
						msg.errch <- nil
						return
					}
				} else {
					if r.Kind == ACK {
						msg.errch <- nil
					} else if r.Kind == NACK {
						msg.errch <- NACKERROR
					}
					return
				}
			}
		}
	}()
	return ch
}

func (this *Wire) getCallback(seq uint32, last bool) chan *Response {
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
		msg.errch <- UnknownTopic(faults.New(UNKNOWNTOPIC, msg.name))
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
		// writing to the wire can be faster (theoretically) than lauching the asynchWaitForCallback
		// so we first prepare the reception if (any)
		var ch chan *Response
		if test(msg.kind, SUB, UNSUB, REQ, REQALL, PUSH) {
			// wait the reply asynchronously
			ch = this.asynchWaitForCallback(msg)
		}
		err := this.write(c, msg)

		if err != nil {
			if ch != nil {
				ch <- nil // stop waiting for calback
			}
			msg.errch <- err
		} else if ch == nil {
			msg.errch <- nil
		}
	}
	// exit
	this.disconnected(c, nil)
}

func (this *Wire) write(c net.Conn, msg Envelope) (err error) {
	defer func() {
		if err != nil {
			this.disconnected(c, err)
		}
	}()

	c.SetWriteDeadline(timeouttime(msg.timeout))

	buf := bufio.NewWriter(c)
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
	r := NewInputStream(c)
	var err error

loop:
	for {
		// we wait forever for the first read
		c.SetReadDeadline(time.Time{})

		// kind
		var k uint8
		k, err = r.ReadUI8()
		if err != nil {
			break
		}
		var kind = EKind(k)

		// after the first read we apply the timeout
		c.SetWriteDeadline(timeouttime(this.timeout))

		// sequence
		var seq uint32
		seq, err = r.ReadUI32()
		if err != nil {
			break
		}

		var name string
		var data []byte

		if test(kind, SUB, UNSUB, REQ, REQALL, PUSH, PUB) {
			if test(kind, SUB, UNSUB) {
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
					break loop
				}
				last = kind == REP

			case ERR, ERR_PARTIAL:
				_, data, err = read(r, false, true)
				if err != nil {
					break loop
				}
				// reply was an error
				var s string
				if this.codec != nil {
					var e = this.codec.Decode(data, &s)
					if e != nil {
						logger.Errorf("[reader] Unable to decode %s; cause=%s", data, e)
						s = fmt.Sprintf("Unable to decode %s; cause=%s", data, e)
					}
					data = []byte(s)
				}
				last = kind == ERR
			}

			cb := this.getCallback(seq, last)
			if cb != nil {
				var r = NewResponse(this, c, kind, seq, data)
				cb <- &r
			} else {
				logger.Errorf("No callback found for kind=%s, sequence=%d.", kind, seq)
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

	this.disconnected(c, err)
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
			var deferReply bool
			var terminate bool
			var handlers = this.findHandlers(msg.name)
			if len(handlers) > 0 {
				var req = NewRequest(this, c, msg)
				for _, hnd := range handlers {
					hnd(req)
					// if there is any handler that terminates,
					// there will be no reply
					if req.terminate {
						terminate = true
					}
					// if there is at least one handler with gomsg.Request
					// there will be no explicit reply,
					if req.deferReply {
						deferReply = true
						reply = nil
					}
					if req.Fault() != nil {
						err = req.Fault() // single fault
						reply = nil
						break
					} else if !deferReply {
						reply = req.Reply() // single reply
					}
					// reset
					req.terminate = false
					req.deferReply = false
				}
			} else {
				err = UnknownTopic(faults.New(UNKNOWNTOPIC, msg.name))
			}

			// only sends a reply if there was some kind of return action
			if terminate {
				this.reply(ACK, msg.sequence, nil)
			} else if !deferReply {
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
			var handlers = this.findHandlers(msg.name)

			if msg.kind == PUSH {
				if len(handlers) > 0 {
					msg.kind = ACK
				} else {
					msg.kind = NACK
				}
				this.reply(msg.kind, msg.sequence, nil)
			}

			if len(handlers) > 0 {
				var req = NewRequest(this, c, msg)
				for _, handler := range handlers {
					handler(req)
				}
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

func CreateResponseHandler(codec Codec, fun interface{}) func(Response) {
	if f, ok := fun.(func(Response)); ok {
		return f
	}

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
			panic("Invalid function. All parameter function are optional but they must folow the signature order: func([gomsg.Request], [custom type], [error]).")
		}
	}

	return func(ctx Response) {
		var p reflect.Value
		var params = make([]reflect.Value, 0)
		if hasContext {
			params = append(params, reflect.ValueOf(ctx))
		}
		if payloadType != nil {
			if codec != nil && ctx.reply != nil {
				p = reflect.New(payloadType)
				var e = codec.Decode(ctx.reply, p.Interface())
				if e != nil {
					logger.Errorf("[deserializeHandshake] Unable to decode %s; cause=%s", ctx.reply, e)
					ctx.fault = e
				}
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
			var err = ctx.Fault()
			params = append(params, reflect.ValueOf(&err).Elem())
		}

		function.Call(params)
	}

}

func CreateRequestHandler(codec Codec, fun interface{}) func(*Request) {
	if f, ok := fun.(func(*Request)); ok {
		return f
	}

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
			panic("Invalid function. In a two paramater function the first must be the gomsg.Request.")
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

	return func(req *Request) {
		var p reflect.Value
		var params = make([]reflect.Value, 0)
		if hasContext {
			params = append(params, reflect.ValueOf(req))
		}
		if payloadType != nil {
			if codec != nil && req.payload != nil {
				p = reflect.New(payloadType)
				var e = codec.Decode(req.payload, p.Interface())
				if e != nil {
					//panic(fmt.Sprintf("[createRequestHandler] Unable to decode %s; cause=%s", req.payload, e))
					logger.Errorf("[createRequestHandler] Unable to decode %s; cause=%s", req.payload, e)
					req.SetFault(e)
					return
				}
				params = append(params, p.Elem())
			} else {
				// when the codec is nil the data is sent as is
				if req.payload == nil {
					params = append(params, reflect.Zero(payloadType))
				} else {
					params = append(params, reflect.ValueOf(req.payload))
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
				req.SetFault(err)
			} else {
				req.SetReply(result)
			}
		}
	}
}

type handler struct {
	rule string
	call func(*Request)
}

type ClientServer struct {
	muhnd    sync.RWMutex
	handlers []handler

	timeout   time.Duration
	muconn    sync.RWMutex
	OnConnect func(w *Wired)
	OnClose   func(c net.Conn)
}

func (this *ClientServer) removeHandler(name string) {
	this.muhnd.Lock()
	defer this.muhnd.Unlock()

	var a = this.handlers
	for i, v := range a {
		if v.rule == name {
			copy(a[i:], a[i+1:])
			a[len(a)-1] = handler{} // avoid memory leak
			this.handlers = a[:len(a)-1]
			return
		}
	}

	this.muhnd.Unlock()
}

func (this *ClientServer) addHandler(name string, hnd func(*Request)) {
	this.muhnd.Lock()
	defer this.muhnd.Unlock()

	for k, v := range this.handlers {
		if v.rule == name {
			this.handlers[k].call = hnd
			return
		}
	}

	this.handlers = append(this.handlers, handler{name, hnd})

}

// SetTimeout sets the timeout used to send data
func (this *ClientServer) SetTimeout(timeout time.Duration) {
	this.timeout = timeout
}

// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func createEnvelope(kind EKind, name string, payload interface{}, success interface{}, timeout time.Duration, codec Codec) (Envelope, error) {
	var handler func(ctx Response)
	if success != nil {
		handler = CreateResponseHandler(codec, success)
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
			msg.payload, err = codec.Encode(payload)
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

// NewClient creates a Client
func NewClient() *Client {
	this := &Client{
		wire:                 NewWire(JsonCodec{}),
		reconnectInterval:    time.Millisecond * 10,
		reconnectMaxInterval: time.Second,
	}
	this.timeout = time.Second * 10
	this.handlers = make([]handler, 0)

	this.wire.findHandlers = func(name string) []func(ctx *Request) {
		this.muhnd.RLock()
		defer this.muhnd.RUnlock()

		return findHandlers(name, this.handlers)
	}

	this.wire.disconnected = func(c net.Conn, e error) {
		this.muconn.Lock()
		defer this.muconn.Unlock()

		// Since this can be called from several places at the same time (reader goroutine, Reconnect(), ),
		// we must check if it is still the same connection
		if this.conn == c {
			this.conn.Close()
			if this.OnClose != nil {
				this.OnClose(this.conn)
			}
			this.conn = nil
			if this.reconnectInterval > 0 {
				go func(interval time.Duration) {
					this.dial(interval, make(chan error, 1))
				}(this.reconnectInterval)
			}
		}

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
	return this
}

// Make it belong to a group.
// Only one element at a time (round-robin) handles the messages.
func (this *Client) SetGroupId(groupId string) *Client {
	this.wire.localGroupID = groupId
	return this
}

func (this *Client) handshake(c net.Conn) error {
	this.muhnd.RLock()
	var size = len(this.handlers) + 1
	var payload = make([]string, size)
	payload[0] = this.wire.localGroupID
	i := 1
	for _, v := range this.handlers {
		payload[i] = v.rule
		i++
	}
	this.muhnd.RUnlock()

	err := serializeHanshake(c, this.wire.codec, this.timeout, payload)
	if err != nil {
		return err
	}

	// get reply
	group, remoteTopics, err := deserializeHandshake(c, this.wire.codec, this.timeout)
	if err != nil {
		return err
	}
	this.wire.mutop.Lock()
	this.wire.remoteTopics = remoteTopics
	this.wire.remoteGroupID = group
	this.wire.mutop.Unlock()

	return nil
}

func serializeHanshake(c net.Conn, codec Codec, timeout time.Duration, payload []string) error {
	var data []byte
	var err error
	if codec != nil {
		data, err = codec.Encode(payload)
		if err != nil {
			return faults.Wrap(err)
		}
	} else {
		var buf = new(bytes.Buffer)
		os := NewOutputStream(buf)
		os.WriteUI16(uint16(len(payload)))
		if err != nil {
			return faults.Wrap(err)
		}
		for _, v := range payload {
			os.WriteString(v)
		}
		data = buf.Bytes()
	}

	c.SetWriteDeadline(timeouttime(timeout))

	buf := bufio.NewWriter(c)
	w := NewOutputStream(buf)
	err = w.WriteBytes(data)
	if err != nil {
		return err
	}
	return buf.Flush()
}

func deserializeHandshake(c net.Conn, codec Codec, timeout time.Duration) (string, map[string]bool, error) {
	c.SetReadDeadline(timeouttime(timeout))

	r := NewInputStream(c)
	data, err := r.ReadBytes()
	if err != nil {
		return "", nil, faults.Wrap(err)
	}
	var topics []string
	if codec != nil {
		var e = codec.Decode(data, &topics)
		if e != nil {
			panic(fmt.Sprintf("[deserializeHandshake] Unable to decode %s; cause=%s", data, e))
		}

	} else {
		var buf = bytes.NewBuffer(data)
		is := NewInputStream(buf)
		size, err := is.ReadUI16()
		if err != nil {
			return "", nil, faults.Wrap(err)
		}
		topics = make([]string, size)
		var length = int(size)
		for i := 0; i < length; i++ {
			topics[i], err = is.ReadString()
			if err != nil {
				return "", nil, faults.Wrap(err)
			}
		}
	}
	var identity = topics[0]
	topics = topics[1:]
	remoteTopics := make(map[string]bool)
	for _, v := range topics {
		remoteTopics[v] = true
	}
	return identity, remoteTopics, nil
}

// connect is seperated to allow the definition and use of OnConnect
func (this *Client) Connect(addr string) <-chan error {
	this.addr = addr
	var cherr = make(chan error, 1)
	this.dial(this.reconnectInterval, cherr)
	return cherr
}

func (this *Client) Address() string {
	return this.addr
}

func (this *Client) Reconnect() <-chan error {
	this.Disconnect()
	return this.Connect(this.addr)
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

func (this *Client) Destroy() {
	this.reconnectInterval = 0
	this.wire.Destroy()
	this.Disconnect()
}

// Active check if this wire is running, i.e., if it has a connection
func (this *Client) Active() bool {
	return this.conn != nil
}

// tries do dial. If dial fails and if reconnectInterval > 0
// it schedules a new try within reconnectInterval milliseconds
func (this *Client) dial(retry time.Duration, cherr chan error) {
	this.muconn.Lock()
	defer this.muconn.Unlock()

	// gets the connection
	c, err := net.DialTimeout("tcp", this.addr, time.Second)
	if err != nil {
		logger.Debugf("[dial] failed to connect to %s", this.addr)
		if this.reconnectInterval > 0 {
			logger.Debugf("[dial] retry in %v", retry)
			go func() {
				time.Sleep(retry)
				if this.reconnectMaxInterval > 0 && retry < this.reconnectMaxInterval {
					retry = retry * 2
					if retry > this.reconnectMaxInterval {
						retry = this.reconnectMaxInterval
					}
				}
				this.dial(retry, cherr)
			}()
		} else {
			logger.Debugf("[dial] NO retry will be performed!")
		}
		return
	}

	logger.Debugf("[dial] connected to %s with %s", this.addr, c.LocalAddr())

	// topic exchange
	err = this.handshake(c)
	if err != nil {
		logger.Errorf("[dial] error while handshaking: %s", err)
		c.Close()
	} else {
		go this.wire.writer(c)
		go this.wire.reader(c)
		go this.wire.runHandler()

		this.conn = c

		if this.OnConnect != nil {
			this.OnConnect(NewWired(c, this.wire))
		}
	}
	cherr <- err
}

func (this *Client) Connection() net.Conn {
	return this.conn
}

// name can have an '*' at the end, meaning that it will handle messages
// with the destiny name starting with the reply name whitout the '*'.
// When handling request messages, the function handler can have a return value and/or an error.
// When handling publish/push messages, any return from the function handler is discarded.
// When handling Request/RequestAll messages, if a return is not specified,
// the caller will not receive a reply until you explicitly call gomsg.Request.ReplyAs()
func (this *Client) Handle(name string, fun interface{}) {
	hnd := CreateRequestHandler(this.wire.codec, fun)
	this.addHandler(name, hnd)

	this.muconn.Lock()
	defer this.muconn.Unlock()

	if this.conn != nil {
		msg, _ := createEnvelope(SUB, name, nil, nil, this.timeout, this.wire.codec)
		this.wire.Send(msg)
	}
}

func (this *Client) Cancel(name string) {
	this.removeHandler(name)

	this.muconn.Lock()
	defer this.muconn.Unlock()

	if this.conn != nil {
		msg, _ := createEnvelope(UNSUB, name, nil, nil, this.timeout, this.wire.codec)
		this.wire.Send(msg)
	}
}

// Request sends a message and waits for the reply
func (this *Client) Request(name string, payload interface{}, handler interface{}) <-chan error {
	return this.RequestTimeout(name, payload, handler, this.timeout)
}

// RequestTimeout is the same as Request with a timeout definition
func (this *Client) RequestTimeout(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQ, name, payload, handler, timeout)
}

// RequestAll requests messages to all connected clients of the same server. If a client is not connected it is forever lost.
func (this *Client) RequestAll(name string, payload interface{}, handler interface{}) <-chan error {
	return this.Send(REQALL, name, payload, handler, this.timeout)
}

// RequestAllTimeout requests messages to all connected clients of the same server. If a client is not connected it is forever lost.
func (this *Client) RequestAllTimeout(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQALL, name, payload, handler, timeout)
}

// Push sends a message and receive an acknowledge
func (this *Client) Push(name string, m interface{}) <-chan error {
	return this.PushTimeout(name, m, this.timeout)
}

// PushTimeout  is the same as Push with a timeout definition
func (this *Client) PushTimeout(name string, m interface{}, timeout time.Duration) <-chan error {
	return this.Send(PUSH, name, m, nil, timeout)
}

// Publish sends a message without any reply
func (this *Client) Publish(name string, m interface{}) <-chan error {
	return this.PublishTimeout(name, m, this.timeout)
}

// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Client) PublishTimeout(name string, m interface{}, timeout time.Duration) <-chan error {
	return this.Send(PUB, name, m, nil, timeout)
}

// When the payload is of type []byte it passes the raw bytes without encoding.
func (this *Client) Send(kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	msg, err := createEnvelope(kind, name, payload, handler, timeout, this.wire.codec)
	if err != nil {
		ch := make(chan error, 1)
		ch <- err
		return ch
	}
	return this.wire.Send(msg)
}

// Wired serves as a read only construct to topic handlers
type Wired struct {
	conn            net.Conn
	wire            *Wire
	onSendListeners map[uint64]SendListener
	sendListenerIdx uint64
}

func NewWired(conn net.Conn, wire *Wire) *Wired {
	return &Wired{
		conn:            conn,
		wire:            wire,
		onSendListeners: make(map[uint64]SendListener),
	}
}

// Conn gets the connection
func (w *Wired) Conn() net.Conn {
	return w.conn
}

//Wire gets the wire
func (w *Wired) Wire() *Wire {
	return w.wire
}

func (w *Wired) Send(msg Envelope) <-chan error {
	// fires all registered listeners
	w.fireSendListener(SendEvent{
		Kind:    msg.kind,
		Name:    msg.name,
		Payload: msg.payload,
		Handler: msg.handler,
	})
	return w.wire.Send(msg)
}

// AddSendListener adds a listener on send messages (Publis/Push/RequestAll/Request)
func (this *Wired) AddSendListener(idx uint64, listener SendListener) uint64 {
	if idx == 0 {
		idx = atomic.AddUint64(&this.sendListenerIdx, 1)
	}
	this.onSendListeners[idx] = listener
	return idx
}

// RemoveSendListener removes a previously added listener on send messages
func (this *Wired) RemoveSendListener(idx uint64) {
	delete(this.onSendListeners, idx)
}

func (this *Wired) fireSendListener(event SendEvent) {
	for _, listener := range this.onSendListeners {
		listener(event)
	}
}

func (this *Wired) Destroy() {
	this.wire.Destroy()
	if this.conn != nil {
		this.conn.Close()
	}
	this.conn = nil
	this.onSendListeners = make(map[uint64]SendListener)
	atomic.StoreUint64(&this.sendListenerIdx, 0)
}

// Wires manages a collection of connections as if they were one.
// Connections are grouped accordingly to its group id.
// A wire with an empty group id means all nodes are different.
//
// Wires with the same non empty id are treated as mirrors of each other.
// This means that we only need to call one of them.
// The other nodes function as High Availability and load balancing nodes
type Wires struct {
	mu              sync.RWMutex
	wires           []*Wired
	groups          map[string]*Group
	cursor          int
	stickies        *Filter
	Codec           Codec
	onSendListeners map[uint64]SendListener
	sendListenerIdx uint64
}

type Group struct {
	wires  []*Wired
	cursor int
}

type sticky struct {
	duration time.Duration
	timeout  time.Time
	lastWire *Wired
}

type SendListener func(event SendEvent)

type SendEvent struct {
	Kind    EKind
	Name    string
	Payload interface{}
	Handler interface{}
}

// AddSendListener adds a listener on send messages (Publis/Push/RequestAll/Request)
func (this *Wires) AddSendListener(idx uint64, listener SendListener) uint64 {
	if idx == 0 {
		idx = atomic.AddUint64(&this.sendListenerIdx, 1)
	}
	this.onSendListeners[idx] = listener
	return idx
}

// RemoveSendListener removes a previously added listener on send messages
func (this *Wires) RemoveSendListener(idx uint64) {
	delete(this.onSendListeners, idx)
}

func (this *Wires) fireSendListener(event SendEvent) {
	for _, listener := range this.onSendListeners {
		listener(event)
	}
}

// NewWires creates a Wires structure
func NewWires(codec Codec) *Wires {
	return &Wires{
		wires:           make([]*Wired, 0),
		groups:          make(map[string]*Group),
		stickies:        NewFilter(),
		Codec:           codec,
		onSendListeners: make(map[uint64]SendListener),
	}
}

// Stick forces the messages to go to the same wire (in a multi wire cenario)
// if the time between messages is smaller than the duration argument.
func (this *Wires) Stick(name string, duration time.Duration) {
	if duration == time.Duration(0) {
		this.stickies.Delete(name)
	} else {
		this.stickies.Put(name, &sticky{duration: duration})
	}
}

func (this *Wires) SetCodec(codec Codec) *Wires {
	this.Codec = codec
	return this
}

func (this *Wires) Destroy() {
	this.mu.Lock()
	defer this.mu.Unlock()

	for _, v := range this.wires {
		v.Destroy()
	}
	this.wires = make([]*Wired, 0)
	this.groups = make(map[string]*Group)
	this.stickies = NewFilter()
	this.onSendListeners = make(map[uint64]SendListener)
	atomic.StoreUint64(&this.sendListenerIdx, 0)
	this.cursor = 0
}

func (this *Wires) Put(conn net.Conn, wire *Wire) *Wired {
	this.mu.Lock()
	defer this.mu.Unlock()

	// set common codec
	wire.codec = this.Codec

	// if already defined (same connection) return it
	for _, v := range this.wires {
		if v.conn == conn {
			v.wire = wire
			return v
		}
	}
	wired := NewWired(conn, wire)
	this.wires = append(this.wires, wired)
	group := this.groups[wire.remoteGroupID]
	if group == nil {
		group = &Group{make([]*Wired, 0), 0}
		this.groups[wire.remoteGroupID] = group
	}
	group.wires = append(group.wires, wired)
	return wired
}

func (this *Wires) Get(conn net.Conn) *Wired {
	return this.Find(func(w *Wired) bool {
		return w.conn == conn
	})
}

func (this *Wires) Find(fn func(w *Wired) bool) *Wired {
	this.mu.Lock()
	defer this.mu.Unlock()

	for _, v := range this.wires {
		if fn(v) {
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
		group := this.groups[w.wire.remoteGroupID]
		if group != nil {
			group.wires, _ = remove(conn, group.wires)
		}
		// remove from stikies
		for _, v := range this.stickies.Items {
			var s = v.Value.(*sticky)
			if s.lastWire == w {
				s.lastWire = nil
			}
		}
		w.Destroy()
	}

}

func remove(conn net.Conn, wires []*Wired) ([]*Wired, *Wired) {
	for k, v := range wires {
		if v.conn == conn {
			// since the slice has a non-primitive, we have to zero it
			copy(wires[k:], wires[k+1:])
			wires[len(wires)-1] = nil // zero it
			wires = wires[:len(wires)-1]
			return wires, v
		}
	}
	return wires, nil
}

func rotate(wires []*Wired, cur int) int {
	cur++
	if cur >= len(wires) {
		cur = 0
	}
	return cur
}

func (this *Wires) GetAll() ([]*Wired, int) {
	this.mu.Lock()
	defer this.mu.Unlock()

	this.cursor = rotate(this.wires, this.cursor)
	return clone(this.wires), this.cursor
}

func (this *Wires) stickyTimeout(name string, wires []*Wired, cur int) (int, bool) {
	if len(wires) == 0 {
		return cur, true
	}

	// find sticky filter that intercepts name
	var match = this.stickies.Match(name)
	if match == nil {
		return rotate(wires, cur), true
	}

	var stick = match.(*sticky)

	var timeout = stick.timeout == time.Time{} ||
		time.Now().After(stick.timeout)
	stick.timeout = time.Now().Add(stick.duration)

	if timeout {
		cur = rotate(wires, cur)
		stick.lastWire = wires[cur]
		return cur, true
	}

	// find cursor for last used wire
	for k, v := range wires {
		if v == stick.lastWire {
			return k, false
		}
	}
	// if we are here, the lastWire is invalid
	stick.lastWire = nil

	return cur, true
}

func (this *Wires) Rotate(name string) ([]*Wired, int) {
	this.mu.Lock()
	defer this.mu.Unlock()

	var cur, ok = this.stickyTimeout(name, this.wires, this.cursor)
	var ws []*Wired
	ws = clone(this.wires)
	// updates original cursor position if rotates
	if ok {
		this.cursor = cur
	}
	return ws, cur
}

func clone(wires []*Wired) []*Wired {
	// clone
	w := make([]*Wired, len(wires))
	copy(w, wires)

	return w
}

// RotateGroups rotates groups
func (this *Wires) RotateGroups(name string) map[string]*Group {
	this.mu.Lock()
	defer this.mu.Unlock()

	newMap := make(map[string]*Group)
	for k, v := range this.groups {
		var cur, ok = this.stickyTimeout(name, v.wires, v.cursor)
		var ws = clone(v.wires)
		// updates original cursor position if rotates
		if ok {
			v.cursor = cur
		}
		newMap[k] = &Group{ws, cur}
	}

	return newMap
}

func (this *Wires) Size() int {
	this.mu.RLock()
	defer this.mu.RUnlock()
	return len(this.wires)
}

// Request sends a message and waits for the reply
// If the type of the payload is *mybus.Msg it will ignore encoding and use the internal bytes as the payload. This is useful if we want to implement a broker.
func (this *Wires) Request(name string, payload interface{}, handler interface{}) <-chan error {
	return this.Send(REQ, name, payload, handler, time.Second)
}

// RequestTimeout sends a message and waits for the reply
func (this *Wires) RequestTimeout(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQ, name, payload, handler, timeout)
}

// RequestAll requests messages to all connected clients. If a client is not connected it is forever lost.
func (this *Wires) RequestAll(name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.Send(REQALL, name, payload, handler, timeout)
}

// Push sends a message and receive an acknowledge
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Wires) Push(name string, payload interface{}) <-chan error {
	return this.Send(PUSH, name, payload, nil, time.Second)
}

// Publish sends a message without any reply
// If the type of the payload is *mybus.Msg
// it will ignore encoding and use the internal bytes as the payload.
func (this *Wires) Publish(name string, payload interface{}) <-chan error {
	return this.Send(PUB, name, payload, nil, time.Second)
}

// Send is the generic function to send messages
// When the payload is of type []byte it passes the raw bytes without encoding.
// When the payload is of type mybus.Msg it passes the FRAMED raw bytes without encoding.
func (this *Wires) Send(kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	return this.SendSkip(nil, kind, name, payload, handler, timeout)
}

// SendSkip is the generic function to send messages with the possibility of ignoring the sender
func (this *Wires) SendSkip(skipWire *Wire, kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error {
	msg, err := createEnvelope(kind, name, payload, handler, timeout, this.Codec)
	errch := make(chan error, 1)
	if err != nil {
		errch <- err
		return errch
	}

	// fires all registered listeners
	this.fireSendListener(SendEvent{
		Kind:    kind,
		Name:    name,
		Payload: payload,
		Handler: handler,
	})

	err = UnknownTopic(faults.New(UNKNOWNTOPIC, msg.name))
	if msg.kind == PUSH || msg.kind == REQ {
		go func() {
			var ws, cursor = this.Rotate(name)
			l := NewLooper(cursor, len(ws))
			for l.HasNext() {
				w := ws[l.Next()]
				// does not send to self
				if skipWire == nil || skipWire != w.wire {
					// REQ can also receive multiple messages from ONE replier
					err = <-w.Send(msg)
					// exit on success
					if err == nil {
						break
					}
				}
			}
			errch <- err
		}()
	} else if msg.kind == PUB {
		go func() {
			// collects wires into groups.
			var groups = this.RotateGroups(name)
			for id, group := range groups {
				if id == "" {
					l := NewLooper(group.cursor, len(group.wires))
					for l.HasNext() {
						w := group.wires[l.Next()]
						// do not send to self
						if skipWire == nil || skipWire != w.wire {
							e := <-w.Send(msg)
							if e == nil {
								err = nil
							}
						}
					}
				} else {
					go func(grp *Group) {
						l := NewLooper(grp.cursor, len(grp.wires))
						for l.HasNext() {
							w := grp.wires[l.Next()]
							// do not send to self
							if skipWire == nil || skipWire != w.wire {
								e := <-w.Send(msg)
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
				if handler != nil {
					handler(ctx)
				}
				// resets the kind
				ctx.Kind = kind
			}
		}

		// collects wires into groups.
		var groups = this.RotateGroups(name)
		for id, group := range groups {
			if id == "" {
				l := NewLooper(group.cursor, len(group.wires))
				for l.HasNext() {
					w := group.wires[l.Next()]
					// do not send to self
					if skipWire == nil || skipWire != w.wire {
						// increment reply counter
						logger.Debugf("[SendSkip] sending message to %s (ungrouped)", w.Conn().RemoteAddr())
						wg.Add(1)
						ch := w.Send(msg)
						go func() {
							// waits for the request completion
							e := <-ch
							if e == nil {
								err = nil // at least one got through
							} else {
								logger.Infof("[SendSkip] %s", e)
							}
							wg.Done()
						}()
					}
				}
			} else {
				// increment reply counter
				wg.Add(1)
				go func(grp *Group) {
					l := NewLooper(grp.cursor, len(grp.wires))
					for l.HasNext() {
						w := grp.wires[l.Next()]
						// do not send to self
						if skipWire == nil || skipWire != w.wire {
							logger.Debugf("[SendSkip] sending message to %s (group %s)", w.Conn().RemoteAddr(), id)
							// waits for the request completion
							e := <-w.Send(msg)
							// send only to one.
							// stop if there was a success.
							if e == nil {
								err = nil
								break
							}
						}
					}
					wg.Done()
				}(group)
			}
		}
		go func() {
			// Wait for all requests to complete.
			wg.Wait()
			logger.Debugf("[SendSkip] all requests finnished")
			// pass the end mark
			if handler != nil {
				handler(NewResponse(skipWire, nil, ACK, 0, nil))
			}
			errch <- err
		}()
	}

	return errch
}

type Server struct {
	ClientServer
	*Wires

	listener net.Listener
}

func NewServer() *Server {
	server := new(Server)
	server.Wires = NewWires(JsonCodec{})
	server.timeout = time.Second * 10
	server.handlers = make([]handler, 0)
	return server
}

// BindAddress returns the listener address
func (this *Server) BindPort() int {
	if this.listener != nil {
		return this.listener.Addr().(*net.TCPAddr).Port
	}
	return 0
}

func (this *Server) Listener() net.Listener {
	return this.listener
}

func (this *Server) Listen(service string) error {
	// listen all interfaces
	l, err := net.Listen("tcp", service)
	if err != nil {
		return err
	}
	this.listener = l
	logger.Debugf("[Listen] listening at %s", l.Addr())
	go func() {
		for {
			// notice that c is changed in the disconnect function
			c, err := l.Accept()
			if err != nil {
				// happens when the listener is closed
				logger.Infof("[Listen] Stoped listening at %s", l.Addr())
				return
			}
			logger.Debugf("[Listen] accepted connection from %s", c.RemoteAddr())

			wire := NewWire(this.Codec)
			wire.findHandlers = this.findHandlers

			// topic exchange
			group, remoteTopics, err := this.handshake(c, wire)

			if err != nil {
				c.Close()
				logger.Errorf("Failed to handshake: %s", err)
			} else {
				wire.remoteGroupID = group
				wire.remoteTopics = remoteTopics
				wire.disconnected = func(conn net.Conn, e error) {
					this.muconn.Lock()
					defer this.muconn.Unlock()

					// check to see if we are disconnecting the same connection
					if c == conn {
						// handle errors during a connection
						if faults.Has(e, io.EOF) || isClosed(e) {
							logger.Infof("[wire.disconnected] client %s closed connection", c.RemoteAddr())
						} else if e != nil {
							logger.Errorf("[wire.disconnected] %s", faults.Wrap(e))
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

func (this *Server) handshake(c net.Conn, wire *Wire) (string, map[string]bool, error) {
	// get remote topics
	group, payload, err := deserializeHandshake(c, wire.codec, this.timeout)
	if err != nil {
		return "", nil, err
	}

	// send identity and local topics
	this.muhnd.RLock()
	var size = len(this.handlers) + 1
	var topics = make([]string, size)
	topics[0] = wire.localGroupID
	i := 1
	for _, v := range this.handlers {
		topics[i] = v.rule
		i++
	}
	this.muhnd.RUnlock()

	err = serializeHanshake(c, wire.codec, this.timeout, topics)
	if err != nil {
		return "", nil, err
	}

	return group, payload, nil
}

func (this *Server) findHandlers(name string) []func(c *Request) {
	this.muhnd.RLock()
	defer this.muhnd.RUnlock()

	return findHandlers(name, this.handlers)
}

// find the first match
func findHandlers(name string, handler []handler) []func(c *Request) {
	var funcs = make([]func(c *Request), 0)
	var prefix string
	for _, v := range handler {
		if strings.HasSuffix(v.rule, FILTER_TOKEN) {
			prefix = v.rule[:len(v.rule)-1]
			if strings.HasPrefix(name, prefix) {
				funcs = append(funcs, v.call)
			}
		} else if name == v.rule {
			funcs = append(funcs, v.call)
		}
	}
	return funcs
}

// Destroy closes all connections and the listener
func (this *Server) Destroy() {
	this.Wires.Destroy()
	if this.listener != nil {
		this.listener.Close()
	}
	this.listener = nil
}

// Handle defines the function that will handle messages for a topic.
// name can have an '*' at the end, meaning that it will handle messages
// with the destiny name starting with the reply name (whitout the '*').
// When handling request messages, the function handler can have a return value and/or an error.
// When handling publish/push messages, any return from the function handler is discarded.
// When handling Request/RequestAll messages, if a return is not specified,
// the caller will not receive a reply until you explicitly call gomsg.Request.ReplyAs()
func (this *Server) Handle(name string, fun interface{}) {
	hnd := CreateRequestHandler(this.Codec, fun)

	this.addHandler(name, hnd)

	msg, _ := createEnvelope(SUB, name, nil, nil, this.timeout, this.Codec)
	ws, cursor := this.Wires.GetAll()
	l := NewLooper(cursor, len(ws))
	for l.HasNext() {
		w := ws[l.Next()]
		w.Send(msg)
	}
}

// Messages are from one client and delivered to another client.
// The sender client does not receive his message.
// The handler execution is canceled. Arriving replies from endpoints are piped to the requesting wire
func (this *Server) Route(name string, timeout time.Duration, before func(x *Request) bool, after func(x *Response)) {
	// This handle is called by the wire.
	// Since this handler has no declared return type, upon return no data will be sent to the caller.
	// Eventually the reply must be sent or a timeout will occur.
	this.Handle(name, func(r *Request) {
		// decide if it should continue (and do other thinhs like store the request data).
		if before != nil && !before(r) {
			return
		}

		wire := r.wire
		this.SendSkip(wire, r.Kind, r.Name, r.Payload(), func(resp Response) {
			if after != nil {
				// do something (ex: store the response data)
				after(&resp)
			}

			wire.reply(resp.Kind, r.sequence, resp.Reply()) // sending data to the caller
		}, timeout)
	})
}

func (this *Server) Cancel(name string) {
	this.removeHandler(name)

	msg, _ := createEnvelope(UNSUB, name, nil, nil, this.timeout, this.Codec)
	ws, cursor := this.Wires.GetAll()
	l := NewLooper(cursor, len(ws))
	for l.HasNext() {
		w := ws[l.Next()]
		w.Send(msg)
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

type Handler interface {
	Handle(name string, fun interface{})
}

type Sender interface {
	Send(kind EKind, name string, payload interface{}, handler interface{}, timeout time.Duration) <-chan error
}

var _ Handler = &Client{}
var _ Handler = &Server{}
var _ Sender = &Client{}
var _ Sender = &Server{}

// Route messages between to different binding ports.
func Route(name string, src Handler, dest Sender, relayTimeout time.Duration, before func(c *Request) bool, after func(c *Response)) {
	src.Handle(name, func(req *Request) {
		if before != nil && !before(req) {
			return
		}

		/*
			There are two ways of doing the following code. Either one is fine (need to test more).
			The first one, implemented here, we wait for the completion of dest.Send() with <-.
			The second one, the commented code, we don't wait with <-
			and we defer the reply with req.DeferReply(),
			and then we send the reply when it comes with req.ReplyAs().
		*/
		// (1rst)
		var kind EKind
		var reply []byte
		// (1rst)
		<-dest.Send(req.Kind, req.Name, req.Payload(), func(resp Response) {
			if after != nil {
				after(&resp)
			}

			// (1rst)
			kind = resp.Kind
			reply = resp.Reply()

			// (2nd)
			//req.ReplyAs(resp.Kind, resp.Reply())
		}, relayTimeout)

		// (1rst)
		req.ReplyAs(kind, reply)

		// (2nd)
		//req.DeferReply()
	})

}

// IcClosed checks for common text messages regarding a closed connection.
// Ugly but can't find another way :(
func isClosed(e error) bool {
	if e != nil {
		var s = e.Error()
		if strings.Contains(s, "EOF") ||
			strings.Contains(s, "use of closed network connection") {
			return true
		}
	}
	return false
}

type Filter struct {
	*KeyValue
}

func NewFilter() *Filter {
	return &Filter{NewKeyValue()}
}

func (filter *Filter) Match(name string) interface{} {
	// find key filter that intercepts name
	for _, item := range filter.Items {
		var s = item.Key.(string)
		if strings.HasSuffix(s, FILTER_TOKEN) {
			if strings.HasPrefix(name, s[:len(s)-1]) {
				return item.Value
			}
		} else if name == s {
			return item.Value
		}
	}
	return nil
}
