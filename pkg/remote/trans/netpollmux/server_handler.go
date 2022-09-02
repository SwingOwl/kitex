/*
 * Copyright 2021 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package netpollmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/cloudwego/netpoll"

	stats2 "github.com/cloudwego/kitex/internal/stats"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/gofunc"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/klog"
	"github.com/cloudwego/kitex/pkg/remote"
	"github.com/cloudwego/kitex/pkg/remote/codec"
	"github.com/cloudwego/kitex/pkg/remote/trans"
	np "github.com/cloudwego/kitex/pkg/remote/trans/netpoll"
	"github.com/cloudwego/kitex/pkg/remote/transmeta"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/serviceinfo"
	"github.com/cloudwego/kitex/pkg/stats"
	"github.com/cloudwego/kitex/transport"
)

var numCPU int

func init() {
	numCPU = runtime.GOMAXPROCS(0)
}

type PacketDetector func(buf []byte) (packetSize int)

type svrTransHandlerFactory struct {
	detector PacketDetector
}

// NewSvrTransHandlerFactoryWithDetector creates a default netpollmux remote.ServerTransHandlerFactory.
func NewSvrTransHandlerFactoryWithDetector(detector PacketDetector) remote.ServerTransHandlerFactory {
	return &svrTransHandlerFactory{detector: detector}
}

// NewSvrTransHandlerFactory creates a default netpollmux remote.ServerTransHandlerFactory.
func NewSvrTransHandlerFactory() remote.ServerTransHandlerFactory {
	return NewSvrTransHandlerFactoryWithDetector(nil)
}

// MuxEnabled returns true to mark svrTransHandlerFactory as a mux server factory.
func (f *svrTransHandlerFactory) MuxEnabled() bool {
	return true
}

// NewTransHandler implements the remote.ServerTransHandlerFactory interface.
// TODO: use object pool?
func (f *svrTransHandlerFactory) NewTransHandler(opt *remote.ServerOption) (remote.ServerTransHandler, error) {
	return newSvrTransHandler(f.detector, opt)
}

func newSvrTransHandler(detector PacketDetector, opt *remote.ServerOption) (*svrTransHandler, error) {
	svrHdlr := &svrTransHandler{
		opt:      opt,
		detector: detector,
		codec:    opt.Codec,
		svcInfo:  opt.SvcInfo,
		ext:      np.NewNetpollConnExtension(),
	}
	if svrHdlr.opt.TracerCtl == nil {
		// init TraceCtl when it is nil, or it will lead some unit tests panic
		svrHdlr.opt.TracerCtl = &stats2.Controller{}
	}
	svrHdlr.funcPool.New = func() interface{} {
		fs := make([]func(), 0, 64) // 64 is defined casually, no special meaning
		return &fs
	}
	return svrHdlr, nil
}

var _ remote.ServerTransHandler = &svrTransHandler{}

type svrTransHandler struct {
	opt        *remote.ServerOption
	detector   PacketDetector
	svcInfo    *serviceinfo.ServiceInfo
	inkHdlFunc endpoint.Endpoint
	codec      remote.Codec
	transPipe  *remote.TransPipeline
	ext        trans.Extension
	funcPool   sync.Pool
	conns      sync.Map
	tasks      sync.WaitGroup
}

// Write implements the remote.ServerTransHandler interface.
func (t *svrTransHandler) Write(ctx context.Context, conn net.Conn, sendMsg remote.Message) (err error) {
	ri := rpcinfo.GetRPCInfo(ctx)
	stats2.Record(ctx, ri, stats.WriteStart, nil)
	defer func() {
		stats2.Record(ctx, ri, stats.WriteFinish, nil)
	}()

	if methodInfo, _ := trans.GetMethodInfo(ri, t.svcInfo); methodInfo != nil {
		if methodInfo.OneWay() {
			return nil
		}
	}
	wbuf := netpoll.NewLinkBuffer()
	bufWriter := np.NewWriterByteBuffer(wbuf)
	err = t.codec.Encode(ctx, sendMsg, bufWriter)
	bufWriter.Release(err)
	if err != nil {
		return err
	}
	conn.(*muxServerConn).Put(func() (buf netpoll.Writer, isNil bool) {
		return wbuf, false
	})
	return nil
}

// Read implements the remote.ServerTransHandler interface.
func (t *svrTransHandler) Read(ctx context.Context, conn net.Conn, msg remote.Message) (err error) {
	return nil
}

// OnRead implements the remote.ServerTransHandler interface.
// Returns write err only.
func (t *svrTransHandler) OnRead(muxSvrConnCtx context.Context, conn net.Conn) error {
	defer t.tryRecover(muxSvrConnCtx, conn)
	connection := conn.(netpoll.Connection)
	r := connection.Reader()

	fs := *t.funcPool.Get().(*[]func())
	for total := r.Len(); total > 0; total = r.Len() {
		buf, err := r.Peek(codec.Size32 * 2)
		if err != nil {
			return destroyConn(connection, err)
		}
		// detect whole packet size
		var packetSize int
		if t.detector != nil {
			packetSize = t.detector(buf)
		}
		// if cannot detect packet boundary, decode packet in order
		// but process message asynchronously
		if packetSize == 0 {
			task, err := t.syncTask(muxSvrConnCtx, conn, r)
			if err != nil {
				return destroyConn(connection, err)
			}
			fs = append(fs, task)
			if len(fs) >= numCPU {
				go t.batchGoTasks(fs)
				fs = *t.funcPool.Get().(*[]func())
			}
			continue
		}

		// if the input buffer < next packet size, batch process message
		if total < packetSize && len(fs) > 0 {
			go t.batchGoTasks(fs)
			fs = *t.funcPool.Get().(*[]func())
		}
		reader, err := r.Slice(packetSize)
		if err != nil {
			return destroyConn(connection, err)
		}
		task, err := t.asyncTask(muxSvrConnCtx, conn, reader)
		if err != nil {
			return destroyConn(connection, err)
		}
		fs = append(fs, task)
	}
	go t.batchGoTasks(fs)
	return nil
}

// batchGoTasks centrally creates goroutines to execute tasks.
func (t *svrTransHandler) batchGoTasks(fs []func()) {
	for n := range fs {
		gofunc.GoFunc(nil, fs[n])
	}
	fs = fs[:0]
	t.funcPool.Put(&fs)
}

func (t *svrTransHandler) syncTask(muxSvrConnCtx context.Context, conn net.Conn, reader netpoll.Reader) (task func(), err error) {
	t.tasks.Add(1)

	// rpcInfoCtx is a pooled ctx with inited RPCInfo which can be reused.
	// it's recycled in defer.
	muxSvrConn, _ := muxSvrConnCtx.Value(ctxKeyMuxSvrConn{}).(*muxServerConn)
	rpcInfo := muxSvrConn.pool.Get().(rpcinfo.RPCInfo)
	rpcInfoCtx := rpcinfo.NewCtxWithRPCInfo(muxSvrConnCtx, rpcInfo)
	// This is the request-level, one-shot ctx.
	// It adds the tracer principally, thus do not recycle.
	ctx := t.startTracer(rpcInfoCtx, rpcInfo)

	recvMsg := remote.NewMessageWithNewer(t.svcInfo, rpcInfo, remote.Call, remote.Server)
	err = t.decodeMessage(ctx, recvMsg, reader)
	if err != nil {
		t.writeErrorReplyIfNeeded(ctx, recvMsg, muxSvrConn, rpcInfo, err, true)
		// for proxy case, need read actual remoteAddr, error print must exec after writeErrorReplyIfNeeded
		t.OnError(ctx, err, muxSvrConn)
		return nil, err
	}
	return func() {
		var closeConn bool
		defer func() {
			panicErr := recover()
			if panicErr != nil {
				if conn != nil {
					ri := rpcinfo.GetRPCInfo(ctx)
					rService, rAddr := getRemoteInfo(ri, conn)
					klog.Errorf("KITEX: panic happened, close conn, remoteAddress=%s remoteService=%s error=%s\nstack=%s", rAddr, rService, panicErr, string(debug.Stack()))
					closeConn = true
				} else {
					klog.Errorf("KITEX: panic happened, error=%s\nstack=%s", panicErr, string(debug.Stack()))
				}
			}
			if closeConn && conn != nil {
				conn.Close()
			}
			t.finishTracer(ctx, rpcInfo, err, panicErr)
			remote.RecycleMessage(recvMsg)

			// reset rpcinfo
			rpcInfo = t.opt.InitOrResetRPCInfoFunc(rpcInfo, conn.RemoteAddr())
			muxSvrConn.pool.Put(rpcInfo)

			t.tasks.Done()
		}()

		closeConn, err = t.processMessage(ctx, muxSvrConn, recvMsg)
	}, nil
}

func (t *svrTransHandler) asyncTask(muxSvrConnCtx context.Context, conn net.Conn, reader netpoll.Reader) (task func(), err error) {
	return func() {
		t.tasks.Add(1)
		defer t.tasks.Done()

		// rpcInfoCtx is a pooled ctx with inited RPCInfo which can be reused.
		// it's recycled in defer.
		muxSvrConn, _ := muxSvrConnCtx.Value(ctxKeyMuxSvrConn{}).(*muxServerConn)
		rpcInfo := muxSvrConn.pool.Get().(rpcinfo.RPCInfo)
		rpcInfoCtx := rpcinfo.NewCtxWithRPCInfo(muxSvrConnCtx, rpcInfo)
		// This is the request-level, one-shot ctx.
		// It adds the tracer principally, thus do not recycle.
		ctx := t.startTracer(rpcInfoCtx, rpcInfo)

		var closeConn bool
		recvMsg := remote.NewMessageWithNewer(t.svcInfo, rpcInfo, remote.Call, remote.Server)
		defer func() {
			panicErr := recover()
			if panicErr != nil {
				if conn != nil {
					ri := rpcinfo.GetRPCInfo(ctx)
					rService, rAddr := getRemoteInfo(ri, conn)
					klog.Errorf("KITEX: panic happened, close conn, remoteAddress=%s remoteService=%s error=%s\nstack=%s", rAddr, rService, panicErr, string(debug.Stack()))
					closeConn = true
				} else {
					klog.Errorf("KITEX: panic happened, error=%s\nstack=%s", panicErr, string(debug.Stack()))
				}
			}
			if closeConn && conn != nil {
				conn.Close()
			}
			t.finishTracer(ctx, rpcInfo, err, panicErr)
			remote.RecycleMessage(recvMsg)

			// reset rpcinfo
			rpcInfo = t.opt.InitOrResetRPCInfoFunc(rpcInfo, conn.RemoteAddr())
			muxSvrConn.pool.Put(rpcInfo)
		}()

		err = t.decodeMessage(ctx, recvMsg, reader)
		if err != nil {
			// No need to close the connection when read failed in this case, because it had finished reads.
			// But still need to close conn if write failed
			closeConn = t.writeErrorReplyIfNeeded(ctx, recvMsg, muxSvrConn, rpcInfo, err, true)
			// for proxy case, need read actual remoteAddr, error print must exec after writeErrorReplyIfNeeded
			t.OnError(ctx, err, muxSvrConn)
			return
		}
		closeConn, err = t.processMessage(ctx, muxSvrConn, recvMsg)
	}, nil
}

func (t *svrTransHandler) decodeMessage(ctx context.Context, msg remote.Message, reader netpoll.Reader) (err error) {
	bufReader := np.NewReaderByteBuffer(reader)
	defer func() {
		if bufReader != nil {
			if err != nil {
				bufReader.Skip(bufReader.ReadableLen())
			}
			bufReader.Release(err)
		}
		stats2.Record(ctx, msg.RPCInfo(), stats.ReadFinish, err)
	}()
	stats2.Record(ctx, msg.RPCInfo(), stats.ReadStart, nil)

	err = t.codec.Decode(ctx, msg, bufReader)
	if err != nil {
		msg.Tags()[remote.ReadFailed] = true
		return err
	}
	return nil
}

func (t *svrTransHandler) processMessage(
	ctx context.Context, muxSvrConn *muxServerConn, msg remote.Message,
) (closeConn bool, err error) {
	var sendMsg remote.Message
	defer remote.RecycleMessage(sendMsg)

	rpcInfo := msg.RPCInfo()
	var methodInfo serviceinfo.MethodInfo
	if methodInfo, err = trans.GetMethodInfo(rpcInfo, t.svcInfo); err != nil {
		closeConn = t.writeErrorReplyIfNeeded(ctx, msg, muxSvrConn, rpcInfo, err, true)
		t.OnError(ctx, err, muxSvrConn)
		return
	}
	if methodInfo.OneWay() {
		sendMsg = remote.NewMessage(nil, t.svcInfo, rpcInfo, remote.Reply, remote.Server)
	} else {
		sendMsg = remote.NewMessage(methodInfo.NewResult(), t.svcInfo, rpcInfo, remote.Reply, remote.Server)
	}

	ctx, err = t.transPipe.OnMessage(ctx, msg, sendMsg)
	if err != nil {
		// error cannot be wrapped to print here, so it must exec before NewTransError
		t.OnError(ctx, err, muxSvrConn)
		err = remote.NewTransError(remote.InternalError, err)
		closeConn = t.writeErrorReplyIfNeeded(ctx, msg, muxSvrConn, rpcInfo, err, false)
		return
	}

	remote.FillSendMsgFromRecvMsg(msg, sendMsg)
	if err = t.transPipe.Write(ctx, muxSvrConn, sendMsg); err != nil {
		t.OnError(ctx, err, muxSvrConn)
		closeConn = true
	}
	return
}

// OnMessage implements the remote.ServerTransHandler interface.
// msg is the decoded instance, such as Arg or Result.
// OnMessage notifies the higher level to process. It's used in async and server-side logic.
func (t *svrTransHandler) OnMessage(ctx context.Context, args, result remote.Message) (context.Context, error) {
	err := t.inkHdlFunc(ctx, args.Data(), result.Data())
	return ctx, err
}

type ctxKeyMuxSvrConn struct{}

// OnActive implements the remote.ServerTransHandler interface.
// sync.Pool for RPCInfo is setup here.
func (t *svrTransHandler) OnActive(ctx context.Context, conn net.Conn) (context.Context, error) {
	connection := conn.(netpoll.Connection)

	// 1. set readwrite timeout
	connection.SetReadTimeout(t.opt.ReadWriteTimeout)

	// 2. set mux server conn
	pool := &sync.Pool{
		New: func() interface{} {
			// init rpcinfo
			ri := t.opt.InitOrResetRPCInfoFunc(nil, connection.RemoteAddr())
			return ri
		},
	}
	muxConn := newMuxServerConn(connection, pool)
	t.conns.Store(conn, muxConn)
	return context.WithValue(context.Background(), ctxKeyMuxSvrConn{}, muxConn), nil
}

// OnInactive implements the remote.ServerTransHandler interface.
func (t *svrTransHandler) OnInactive(ctx context.Context, conn net.Conn) {
	t.conns.Delete(conn)
}

func (t *svrTransHandler) GracefulShutdown(ctx context.Context) error {
	// Send a control frame with sequence ID 0 to notify the remote
	// end to close the connection or prevent further operation on it.
	iv := rpcinfo.NewInvocation("none", "none")
	iv.SetSeqID(0)
	ri := rpcinfo.NewRPCInfo(nil, nil, iv, nil, nil)
	data := NewControlFrame()
	msg := remote.NewMessage(data, t.svcInfo, ri, remote.Reply, remote.Server)
	msg.SetProtocolInfo(remote.NewProtocolInfo(transport.TTHeader, serviceinfo.Thrift))
	msg.TransInfo().TransStrInfo()[transmeta.HeaderConnectionReadyToReset] = "1"

	t.conns.Range(func(k, v interface{}) bool {
		wbuf := netpoll.NewLinkBuffer()
		bufWriter := np.NewWriterByteBuffer(wbuf)
		err := t.codec.Encode(ctx, msg, bufWriter)
		bufWriter.Release(err)
		if err == nil {
			v.(*muxServerConn).Put(func() (buf netpoll.Writer, isNil bool) {
				return wbuf, false
			})
		} else {
			c := v.(*muxServerConn)
			klog.Warn("KITEX: signal connection closing error:",
				err.Error(), c.LocalAddr().String(), "=>", c.RemoteAddr().String())
		}
		return true
	})
	// wait until all notifications are sent and clients stop using those connections
	done := make(chan struct{})
	go func() {
		t.tasks.Wait()
		close(done)
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		}
	}
}

// OnError implements the remote.ServerTransHandler interface.
func (t *svrTransHandler) OnError(ctx context.Context, err error, conn net.Conn) {
	ri := rpcinfo.GetRPCInfo(ctx)
	rService, rAddr := getRemoteInfo(ri, conn)
	if t.ext.IsRemoteClosedErr(err) {
		// it should not regard error which cause by remote connection closed as server error
		if ri == nil {
			return
		}
		remote := rpcinfo.AsMutableEndpointInfo(ri.From())
		remote.SetTag(rpcinfo.RemoteClosedTag, "1")
	} else {
		var de *kerrors.DetailedError
		if ok := errors.As(err, &de); ok && de.Stack() != "" {
			klog.CtxErrorf(ctx, "KITEX: processing request error, remoteService=%s, remoteAddr=%v, error=%s\nstack=%s", rService, rAddr, err.Error(), de.Stack())
		} else {
			klog.CtxErrorf(ctx, "KITEX: processing request error, remoteService=%s, remoteAddr=%v, error=%s", rService, rAddr, err.Error())
		}
	}
}

// SetInvokeHandleFunc implements the remote.InvokeHandleFuncSetter interface.
func (t *svrTransHandler) SetInvokeHandleFunc(inkHdlFunc endpoint.Endpoint) {
	t.inkHdlFunc = inkHdlFunc
}

// SetPipeline implements the remote.ServerTransHandler interface.
func (t *svrTransHandler) SetPipeline(p *remote.TransPipeline) {
	t.transPipe = p
}

func (t *svrTransHandler) writeErrorReplyIfNeeded(
	ctx context.Context, recvMsg remote.Message, conn net.Conn, ri rpcinfo.RPCInfo, err error, doOnMessage bool,
) (shouldCloseConn bool) {
	if methodInfo, _ := trans.GetMethodInfo(ri, t.svcInfo); methodInfo != nil {
		if methodInfo.OneWay() {
			return
		}
	}
	transErr, isTransErr := err.(*remote.TransError)
	if !isTransErr {
		return
	}
	errMsg := remote.NewMessage(transErr, t.svcInfo, ri, remote.Exception, remote.Server)
	remote.FillSendMsgFromRecvMsg(recvMsg, errMsg)
	if doOnMessage {
		// if error happen before normal OnMessage, exec it to transfer header trans info into rpcinfo
		t.transPipe.OnMessage(ctx, recvMsg, errMsg)
	}
	err = t.transPipe.Write(ctx, conn, errMsg)
	if err != nil {
		klog.CtxErrorf(ctx, "KITEX: write error reply failed, remote=%s, error=%s", conn.RemoteAddr(), err.Error())
		return true
	}
	return
}

func (t *svrTransHandler) tryRecover(ctx context.Context, conn net.Conn) {
	if err := recover(); err != nil {
		// rpcStat := internal.AsMutableRPCStats(t.rpcinfo.Stats())
		// rpcStat.SetPanicked(err)
		// t.opt.TracerCtl.DoFinish(ctx, klog)
		// 这里不需要 Reset rpcStats 因为连接会关闭，会直接把 RPCInfo 进行 Recycle

		if conn != nil {
			conn.Close()
			klog.CtxErrorf(ctx, "KITEX: panic happened, close conn[%s], %s\n%s", conn.RemoteAddr(), err, string(debug.Stack()))
		} else {
			klog.CtxErrorf(ctx, "KITEX: panic happened, %s\n%s", err, string(debug.Stack()))
		}
	}
}

func (t *svrTransHandler) startTracer(ctx context.Context, ri rpcinfo.RPCInfo) context.Context {
	c := t.opt.TracerCtl.DoStart(ctx, ri)
	return c
}

func (t *svrTransHandler) finishTracer(ctx context.Context, ri rpcinfo.RPCInfo, err error, panicErr interface{}) {
	rpcStats := rpcinfo.AsMutableRPCStats(ri.Stats())
	if rpcStats == nil {
		return
	}
	if panicErr != nil {
		rpcStats.SetPanicked(panicErr)
	}
	if errors.Is(err, netpoll.ErrConnClosed) {
		// it should not regard error which cause by remote connection closed as server error
		err = nil
	}
	t.opt.TracerCtl.DoFinish(ctx, ri, err)
	// for server side, rpcinfo is reused on connection, clear the rpc stats info but keep the level config
	sl := ri.Stats().Level()
	rpcStats.Reset()
	rpcStats.SetLevel(sl)
}

func getRemoteInfo(ri rpcinfo.RPCInfo, conn net.Conn) (string, net.Addr) {
	rAddr := conn.RemoteAddr()
	if ri == nil {
		return "", rAddr
	}
	if rAddr.Network() == "unix" {
		if ri.From().Address() != nil {
			rAddr = ri.From().Address()
		}
	}
	return ri.From().ServiceName(), rAddr
}

func destroyConn(conn netpoll.Connection, err error) error {
	err = fmt.Errorf("%w: addr(%s)", err, conn.RemoteAddr())
	klog.Errorf("KITEX: error=%s", err.Error())
	conn.Close()
	return err
}
