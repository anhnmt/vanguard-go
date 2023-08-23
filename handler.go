// Copyright 2023 Buf Technologies, Inc.
//
// All rights reserved.

//nolint:forbidigo,revive,gocritic // this is temporary, will be removed when implementation is complete
package vanguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type handler struct {
	mux           *Mux
	bufferPool    *bufferPool
	codecs        map[codecKey]Codec
	canDecompress []string
}

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// Identify the protocol.
	clientProtoHandler, originalContentType, queryVars := classifyRequest(request)
	if clientProtoHandler == nil {
		http.Error(writer, "could not classify protocol", http.StatusUnsupportedMediaType)
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)
	op := operation{
		muxConfig:     h.mux,
		writer:        writer,
		request:       request,
		contentType:   originalContentType,
		cancel:        cancel,
		bufferPool:    h.bufferPool,
		canDecompress: h.canDecompress,
	}
	op.client.protocol = clientProtoHandler
	if queryVars != nil {
		// memoize this, so we don't have to parse query string again later
		op.queryVars = queryVars
	}
	originalHeaders := request.Header.Clone()

	// Identify the method being invoked.
	methodConf, httpErr := h.findMethod(&op)
	if httpErr != nil {
		if httpErr.headers != nil {
			httpErr.headers(writer.Header())
		}
		http.Error(writer, http.StatusText(httpErr.code), httpErr.code)
		return
	}
	op.method = methodConf.descriptor
	op.methodPath = methodConf.methodPath
	op.delegate = methodConf.handler
	op.resolver = methodConf.resolver
	switch {
	case op.method.IsStreamingClient() && op.method.IsStreamingServer():
		op.streamType = connect.StreamTypeBidi
	case op.method.IsStreamingClient():
		op.streamType = connect.StreamTypeClient
	case op.method.IsStreamingServer():
		op.streamType = connect.StreamTypeServer
	default:
		op.streamType = connect.StreamTypeUnary
	}
	if !op.client.protocol.acceptsStreamType(&op, op.streamType) {
		http.Error(
			writer,
			fmt.Sprintf("stream type %s not supported with %s protocol", op.streamType, op.client.protocol),
			http.StatusUnsupportedMediaType)
		return
	}
	if op.streamType == connect.StreamTypeBidi && request.ProtoMajor < 2 {
		http.Error(writer, "bidi streams require HTTP/2", http.StatusHTTPVersionNotSupported)
		return
	}

	// Identify the request encoding and compression.
	reqMeta, err := clientProtoHandler.extractProtocolRequestHeaders(&op, request.Header)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	op.reqMeta = reqMeta
	var cannotDecompressRequest bool
	if reqMeta.compression == CompressionIdentity {
		reqMeta.compression = "" // normalize to empty string
	}
	if reqMeta.compression != "" {
		var ok bool
		op.client.reqCompression, ok = h.mux.compressionPools[reqMeta.compression]
		if !ok {
			// This might be okay, like if the transformation doesn't require decoding.
			op.client.reqCompression = nil
			cannotDecompressRequest = true
		}
	}
	op.client.codec = h.codecs[codecKey{res: methodConf.resolver, name: reqMeta.codec}]
	if op.client.codec == nil {
		http.Error(writer, fmt.Sprintf("%q sub-format not supported", reqMeta.codec), http.StatusUnsupportedMediaType)
		return
	}

	// Now we can determine the destination protocol details
	if _, supportsProtocol := methodConf.protocols[clientProtoHandler.protocol()]; supportsProtocol {
		op.server.protocol = clientProtoHandler.protocol().serverHandler(&op)
	} else {
		for protocol := protocolMin; protocol <= protocolMax; protocol++ {
			if _, supportsProtocol := methodConf.protocols[protocol]; supportsProtocol {
				op.server.protocol = protocol.serverHandler(&op)
				break
			}
		}
	}

	if op.server.protocol.protocol() == ProtocolREST {
		// REST always uses JSON.
		// TODO: allow non-JSON encodings with REST? Would require registering content-types with codecs.
		//
		// NB: This is fine to set even if a custom content-type is used via
		//     the use of google.api.HttpBody. The actual content-type and body
		//     data will be written via serverBodyPreparer implementation.
		op.server.codec = h.mux.codecImpls[CodecJSON](methodConf.resolver)
	} else if _, supportsCodec := methodConf.codecNames[reqMeta.codec]; supportsCodec {
		op.server.codec = op.client.codec
	} else {
		op.server.codec = h.codecs[codecKey{res: methodConf.resolver, name: methodConf.preferredCodec}]
	}

	if reqMeta.compression != "" && !cannotDecompressRequest {
		if _, supportsCompression := methodConf.compressorNames[reqMeta.compression]; supportsCompression {
			op.server.reqCompression = op.client.reqCompression
		} // else: no compression
	}

	// Now we know enough to handle the request.
	if op.client.protocol.protocol() == op.server.protocol.protocol() &&
		op.client.codec.Name() == op.server.codec.Name() &&
		(cannotDecompressRequest || op.client.reqCompression.Name() == op.server.reqCompression.Name()) {
		// No transformation needed. But we do  need to restore the original headers first
		// since extracting request metadata may have removed keys.
		request.Header = originalHeaders
		methodConf.handler.ServeHTTP(writer, request)
		return
	}

	if cannotDecompressRequest {
		// At this point, we have to perform some transformation, so we'll need to
		// be able to decompress/compress.
		http.Error(writer, fmt.Sprintf("%q compression not supported", reqMeta.compression), http.StatusUnsupportedMediaType)
		return
	}

	op.handle()
}

func (h *handler) findMethod(op *operation) (*methodConfig, *httpError) {
	uriPath := op.request.URL.Path
	switch op.client.protocol.protocol() {
	case ProtocolREST:
		var methods routeMethods
		op.restTarget, op.restVars, methods = h.mux.restRoutes.match(uriPath, op.request.Method)
		if op.restTarget != nil {
			return op.restTarget.config, nil
		}
		if len(methods) == 0 {
			return nil, &httpError{code: http.StatusNotFound}
		}
		var sb strings.Builder
		for method := range methods {
			if sb.Len() > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(method)
		}
		return nil, &httpError{
			code: http.StatusMethodNotAllowed,
			headers: func(hdrs http.Header) {
				hdrs.Set("Allow", sb.String())
			},
		}
	default:
		// The other protocols just use the URI path as the method name and don't allow query params
		if len(uriPath) == 0 || uriPath[0] != '/' {
			// no starting slash? won't match any known route
			return nil, &httpError{code: http.StatusNotFound}
		}
		methodConf := h.mux.methods[uriPath[1:]]
		if methodConf == nil {
			// TODO: if the service is known, but the method is not, we should send to the client
			//       a proper RPC error (encoded per protocol handler) with an Unimplemented code.
			return nil, &httpError{code: http.StatusNotFound}
		}
		if op.request.Method != http.MethodPost {
			mayAllowGet, ok := op.client.protocol.(clientProtocolAllowsGet)
			allowsGet := ok && mayAllowGet.allowsGetRequests(methodConf)
			if !allowsGet {
				return nil, &httpError{
					code: http.StatusMethodNotAllowed,
					headers: func(hdrs http.Header) {
						hdrs.Set("Allow", http.MethodPost)
					},
				}
			}
			if allowsGet && op.request.Method != http.MethodGet {
				return nil, &httpError{
					code: http.StatusMethodNotAllowed,
					headers: func(hdrs http.Header) {
						hdrs.Set("Allow", http.MethodGet+","+http.MethodPost)
					},
				}
			}
		}
		return methodConf, nil
	}
}

type clientProtocolDetails struct {
	protocol       clientProtocolHandler
	codec          Codec
	reqCompression *compressionPool
}

type serverProtocolDetails struct {
	protocol       serverProtocolHandler
	codec          Codec
	reqCompression *compressionPool
}

func classifyRequest(req *http.Request) (h clientProtocolHandler, contentType string, values url.Values) {
	contentTypes := req.Header["Content-Type"]

	if len(contentTypes) == 0 { //nolint:nestif
		// Empty bodies should still have content types. So this should only
		// happen for requests with NO body at all. That's only allowed for
		// REST calls and Connect GET calls.
		connectVersion := req.Header["Connect-Protocol-Version"]
		// If this header is present, the intent is clear. But Connect GET
		// requests should actually encode this via query string (see below).
		if len(connectVersion) == 1 && connectVersion[0] == "1" {
			if req.Method == http.MethodGet {
				return connectUnaryGetClientProtocol{}, "", nil
			}
			return nil, "", nil
		}
		vals := req.URL.Query()
		if vals.Get("connect") == "v1" {
			if req.Method == http.MethodGet {
				return connectUnaryGetClientProtocol{}, "", nil
			}
			return nil, "", nil
		}
		return restClientProtocol{}, "", vals
	}

	if len(contentTypes) > 1 {
		return nil, "", nil // Ick. Don't allow this.
	}
	contentType = contentTypes[0]
	switch {
	case strings.HasPrefix(contentType, "application/connect+"):
		return connectStreamClientProtocol{}, contentType, nil
	case contentType == "application/grpc" || strings.HasPrefix(contentType, "application/grpc+"):
		return grpcClientProtocol{}, contentType, nil
	case contentType == "application/grpc-web" || strings.HasPrefix(contentType, "application/grpc-web+"):
		return grpcWebClientProtocol{}, contentType, nil
	case strings.HasPrefix(contentType, "application/"):
		connectVersion := req.Header["Connect-Protocol-Version"]
		if len(connectVersion) == 1 && connectVersion[0] == "1" {
			if req.Method == http.MethodGet {
				return connectUnaryGetClientProtocol{}, contentType, nil
			}
			return connectUnaryPostClientProtocol{}, contentType, nil
		}
		// REST usually uses application/json, but use of google.api.HttpBody means it could
		// also use *any* content-type.
		fallthrough
	default:
		return restClientProtocol{}, contentType, nil
	}
}

type codecKey struct {
	res  TypeResolver
	name string
}

func newCodecMap(methodConfigs map[string]*methodConfig, codecs map[string]func(TypeResolver) Codec) map[codecKey]Codec {
	result := make(map[codecKey]Codec, len(codecs))
	for _, conf := range methodConfigs {
		for codecName, codecFactory := range codecs {
			key := codecKey{res: conf.resolver, name: codecName}
			if _, exists := result[key]; !exists {
				result[key] = codecFactory(conf.resolver)
			}
		}
	}
	return result
}

type httpError struct {
	code    int
	headers func(header http.Header)
}

// operation represents a single HTTP operation, which maps to an incoming HTTP request.
// It tracks properties needed to implement protocol transformation.
type operation struct {
	muxConfig     *Mux
	writer        http.ResponseWriter
	request       *http.Request
	queryVars     url.Values
	contentType   string // original content-type in incoming request headers
	reqMeta       requestMeta
	cancel        context.CancelFunc
	bufferPool    *bufferPool
	delegate      http.Handler
	resolver      TypeResolver
	canDecompress []string

	method     protoreflect.MethodDescriptor
	methodPath string
	streamType connect.StreamType

	client clientProtocolDetails
	server serverProtocolDetails
	// response compression won't vary between response received from
	// server and response sent to client because we tell server handler
	// that we only accept encodings that both the middleware and the
	// client can decompress.
	respCompression *compressionPool

	// only used when clientProtocolDetails.protocol == ProtocolREST
	restTarget *routeTarget
	restVars   []routeTargetVarMatch

	// these fields memoize the results of type assertions and some method calls
	clientEnveloper     envelopedProtocolHandler
	clientPreparer      clientBodyPreparer
	clientReqNeedsPrep  bool
	clientRespNeedsPrep bool
	serverEnveloper     serverEnvelopedProtocolHandler
	serverPreparer      serverBodyPreparer
	serverReqNeedsPrep  bool
	serverRespNeedsPrep bool
}

func (op *operation) queryValues() url.Values {
	if op.queryVars == nil && op.request.URL.RawQuery != "" {
		op.queryVars = op.request.URL.Query()
	}
	return op.queryVars
}

func (op *operation) handle() {
	op.clientEnveloper, _ = op.client.protocol.(envelopedProtocolHandler)
	op.clientPreparer, _ = op.client.protocol.(clientBodyPreparer)
	if op.clientPreparer != nil {
		op.clientReqNeedsPrep = op.clientPreparer.requestNeedsPrep(op)
		op.clientRespNeedsPrep = op.clientPreparer.responseNeedsPrep(op)
	}
	op.serverEnveloper, _ = op.server.protocol.(serverEnvelopedProtocolHandler)
	op.serverPreparer, _ = op.server.protocol.(serverBodyPreparer)
	if op.serverPreparer != nil {
		op.serverReqNeedsPrep = op.serverPreparer.requestNeedsPrep(op)
		op.serverRespNeedsPrep = op.serverPreparer.responseNeedsPrep(op)
	}

	serverRequestBuilder, _ := op.server.protocol.(requestLineBuilder)
	var requireMessageForRequestLine bool
	if serverRequestBuilder != nil {
		requireMessageForRequestLine = serverRequestBuilder.requiresMessageToProvideRequestLine(op)
	}

	sameRequestCompression := op.client.reqCompression.Name() == op.server.reqCompression.Name()
	sameCodec := op.client.codec.Name() == op.server.codec.Name()
	// even if body encoding uses same content type, we can't treat them as the same
	// (which means re-using encoded data) if either side needs to prep the data first
	sameRequestCodec := sameCodec && !op.clientReqNeedsPrep && !op.serverReqNeedsPrep
	mustDecodeRequest := !sameRequestCodec || requireMessageForRequestLine

	reqMsg := message{
		sameCompression: sameRequestCompression,
		sameCodec:       sameRequestCodec,
	}

	if mustDecodeRequest {
		// Need the message type to decode
		messageType, err := op.resolver.FindMessageByName(op.method.Input().FullName())
		if err != nil {
			op.earlyError(err)
			return
		}
		reqMsg.msg = messageType.New().Interface()
	}

	var skipBody bool
	if serverRequestBuilder != nil { //nolint:nestif
		if requireMessageForRequestLine {
			if err := op.readRequestMessage(op.request.Body, &reqMsg); err != nil {
				op.earlyError(err)
				return
			}
			if err := reqMsg.advanceToStage(op, stageDecoded); err != nil {
				op.earlyError(err)
				return
			}
		}
		var hasBody bool
		var err error
		op.request.URL.Path, op.request.URL.RawQuery, op.request.Method, hasBody, err =
			serverRequestBuilder.requestLine(op, reqMsg.msg)
		if err != nil {
			op.earlyError(err)
			return
		}
		skipBody = !hasBody
	} else {
		// if no request line builder, use simple request layout
		op.request.URL.Path = op.methodPath
		op.request.URL.RawQuery = ""
		op.request.Method = http.MethodPost
	}
	op.request.URL.ForceQuery = false
	svrReqMeta := op.reqMeta
	svrReqMeta.codec = op.server.codec.Name()
	svrReqMeta.compression = op.server.reqCompression.Name()
	svrReqMeta.acceptCompression = intersect(op.reqMeta.acceptCompression, op.canDecompress)
	op.server.protocol.addProtocolRequestHeaders(svrReqMeta, op.request.Header)

	// Now we can define the transformed request body.
	if skipBody {
		// drain any contents of body so downstream handler sees empty
		op.drainBody(op.request.Body)
	} else {
		if sameRequestCompression && sameRequestCodec && !mustDecodeRequest {
			// we do not need to decompress or decode
			op.request.Body = op.serverBody(nil)
		} else {
			op.request.Body = op.serverBody(&reqMsg)
		}
	}

	// Finally, define the transforming response writer (which
	// must delay most logic until it sees WriteHeader).
	rw, err := op.serverWriter()
	if err != nil {
		op.earlyError(err)
	}
	defer rw.close()
	op.writer = rw
	op.delegate.ServeHTTP(op.writer, op.request)
}

// earlyError handles an error that occurs while setting up the operation. It should not be used
// once the underlying server handler has been invoked. For those errors, responseWriter.reportError
// must be used instead.
func (op *operation) earlyError(_ error) {
	// TODO: determine status code from error
	// TODO: if a *connect.Error, use protocol handler to write RPC response
	http.Error(op.writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
}

func (op *operation) readRequestMessage(reader io.Reader, msg *message) error {
	msgLen := -1
	compressed := op.client.reqCompression != nil
	if op.clientEnveloper != nil {
		var envBuf envelopeBytes
		_, err := io.ReadFull(reader, envBuf[:])
		if err != nil {
			return err
		}
		env, err := op.clientEnveloper.decodeEnvelope(envBuf)
		if err != nil {
			return err
		}
		if env.trailer {
			return fmt.Errorf("client stream cannot include status/trailer message")
		}
		msgLen, compressed = int(env.length), env.compressed
	}

	buffer := msg.reset(op.bufferPool, true, compressed)
	var err error
	if msgLen == -1 {
		// TODO: apply some limit to request message size to avoid unlimited memory use
		_, err = io.Copy(buffer, reader)
	} else {
		_, err = io.CopyN(buffer, reader, int64(msgLen))
		if errors.Is(err, io.EOF) {
			// EOF is a sentinel that means normal end of stream; replace it so callers know an error occurred
			err = io.ErrUnexpectedEOF
		}
	}
	if err != nil {
		return err
	}
	msg.stage = stageRead
	return nil
}

func (op *operation) serverBody(msg *message) io.ReadCloser {
	if msg == nil {
		// no need to decompress or decode; just transforming envelopes
		return &envelopingReader{op: op, r: op.request.Body}
	}
	ret := &transformingReader{op: op, msg: msg, r: op.request.Body}
	if msg.stage != stageEmpty {
		if err := ret.prepareMessage(); err != nil {
			ret.err = err
		}
	}
	return ret
}

func (op *operation) serverWriter() (*responseWriter, error) {
	flusher, ok := op.writer.(http.Flusher)
	if !ok {
		return nil, errors.New("http.ResponseWriter must implement http.Flusher")
	}
	return &responseWriter{op: op, delegate: op.writer, flusher: flusher}, nil
}

func (op *operation) drainBody(body io.ReadCloser) {
	if wt, ok := body.(io.WriterTo); ok {
		_, _ = wt.WriteTo(io.Discard)
		return
	}
	buf := op.bufferPool.Get()
	defer op.bufferPool.Put(buf)
	b := buf.Bytes()[0:buf.Cap()]
	_, _ = io.CopyBuffer(io.Discard, body, b)
}

// envelopingReader will translate between envelope styles as data is read.
// It does not do any decompressing or deserializing of data.
type envelopingReader struct {
	op *operation
	r  io.ReadCloser
}

func (er envelopingReader) Read(data []byte) (n int, err error) {
	//TODO implement me
	panic("implement me")
}

func (er envelopingReader) Close() error {
	//TODO implement me
	panic("implement me")
}

// transformingReader transforms the data from the original request
// into a new protocol form as the data is read. It must decompress
// and deserialize each message and then re-serialize (and optionally
// recompress) each message. Since the original incoming protocol may
// have different envelope conventions than the outgoing protocol, it
// also rewrites envelopes.
type transformingReader struct {
	op  *operation
	msg *message
	r   io.ReadCloser

	err       error
	buffer    *bytes.Buffer
	env       envelopeBytes
	envRemain int
}

func (tr *transformingReader) Read(data []byte) (n int, err error) {
	if tr.err != nil {
		return 0, tr.err
	}
	if tr.buffer != nil {
		n, err := tr.buffer.Read(data)
		if n > 0 {
			return n, err
		}
		// otherwise EOF, fall through
	}
	if err := tr.op.readRequestMessage(tr.r, tr.msg); err != nil {
		tr.err = err
		return 0, err
	}
	if err := tr.prepareMessage(); err != nil {
		tr.err = err
		return 0, err
	}

	if len(data) < tr.envRemain {
		copy(data, tr.env[envelopeLen-tr.envRemain:])
		tr.envRemain -= len(data)
		return len(data), nil
	}
	var offset int
	if tr.envRemain > 0 {
		copy(data, tr.env[envelopeLen-tr.envRemain:])
		offset = tr.envRemain
		tr.envRemain = 0
	}
	if len(data) > offset {
		n, err = tr.buffer.Read(data[offset:])
	}
	return offset + n, err
}

func (tr *transformingReader) Close() error {
	tr.err = errors.New("body is closed")
	tr.msg.release(tr.op.bufferPool)
	return tr.r.Close()
}

func (tr *transformingReader) prepareMessage() error {
	if err := tr.msg.advanceToStage(tr.op, stageSend); err != nil {
		return err
	}
	tr.buffer = tr.msg.sendBuffer()
	if tr.op.serverEnveloper == nil {
		tr.envRemain = 0
		return nil
	}
	// Need to prefix the buffer with an envelope
	env := envelope{
		compressed: tr.msg.wasCompressed,
		length:     uint32(tr.buffer.Len()),
	}
	tr.env = tr.op.serverEnveloper.encodeEnvelope(env)
	tr.envRemain = envelopeLen
	return nil
}

// responseWriter wraps the original writer and performs the protocol
// transformation. When headers and data are written to this writer,
// they may be modified before being written to the underlying writer,
// which accomplishes the protocol change.
//
// When the headers are written, the actual transformation that is
// needed is determined and a writer decorator created.
type responseWriter struct {
	op       *operation
	delegate http.ResponseWriter
	flusher  http.Flusher
	code     int
	// has WriteHeader or first call to Write occurred?
	headersWritten bool
	// have headers actually been flushed to delegate?
	headersFlushed bool
	// have we already written the end of the stream (error/trailers/etc)?
	endWritten bool
	respMeta   *responseMeta
	err        error
	// wraps op.writer; initialized after headers are written
	w io.WriteCloser
}

func (rw *responseWriter) Header() http.Header {
	return rw.delegate.Header()
}

func (rw *responseWriter) Write(data []byte) (int, error) {
	if !rw.headersWritten {
		rw.WriteHeader(http.StatusOK)
	}
	if rw.err != nil {
		return 0, rw.err
	}
	return rw.w.Write(data)
}

func (rw *responseWriter) WriteHeader(statusCode int) {
	if rw.headersWritten {
		return
	}
	rw.headersWritten = true
	rw.code = statusCode
	respMeta, processBody, err := rw.op.server.protocol.extractProtocolResponseHeaders(statusCode, rw.Header())
	if err != nil {
		rw.reportError(err)
		return
	}
	rw.respMeta = &respMeta
	if respMeta.compression == CompressionIdentity {
		respMeta.compression = "" // normalize to empty string
	}
	if respMeta.compression != "" {
		var ok bool
		rw.op.respCompression, ok = rw.op.muxConfig.compressionPools[respMeta.compression]
		if !ok {
			rw.reportError(fmt.Errorf("response indicates unsupported compression encoding %q", respMeta.compression))
			return
		}
	}
	if respMeta.codec != "" && respMeta.codec != rw.op.server.codec.Name() {
		// unexpected content-type for reply
		rw.reportError(fmt.Errorf("response uses incorrect codec: expecting %q but instead got %q", rw.op.server.codec.Name(), respMeta.codec))
		return
	}

	if respMeta.end != nil {
		// RPC failed immediately.
		if processBody != nil {
			// We have to wait until we receive the body in order to process the error.
			rw.w = &errorWriter{
				rw:          rw,
				respMeta:    rw.respMeta,
				processBody: processBody,
				buffer:      rw.op.bufferPool.Get(),
			}
			return
		}
		// We can send back error response immediately.
		rw.flushHeaders()
		return
	}

	sameCodec := rw.op.client.codec.Name() == rw.op.server.codec.Name()
	// even if body encoding uses same content type, we can't treat them as the same
	// (which means re-using encoded data) if either side needs to prep the data first
	sameResponseCodec := sameCodec && !rw.op.clientRespNeedsPrep && !rw.op.serverRespNeedsPrep

	respMsg := message{sameCompression: true, sameCodec: sameResponseCodec}

	if !sameResponseCodec {
		// We will have to decode and re-encode, so we need the message type.
		messageType, err := rw.op.resolver.FindMessageByName(rw.op.method.Output().FullName())
		if err != nil {
			rw.reportError(err)
			return
		}
		respMsg.msg = messageType.New().Interface()
	}

	var endMustBeInHeaders bool
	if mustBe, ok := rw.op.client.protocol.(clientProtocolEndMustBeInHeaders); ok {
		endMustBeInHeaders = mustBe.endMustBeInHeaders()
	}
	if !endMustBeInHeaders {
		// We can go ahead and flush headers now. Otherwise, we'll wait until we've verified we
		// can handle the response data, so we still have an opportunity to send back an error.
		rw.flushHeaders()
	}

	// Now we can define the transformed response body.
	if sameResponseCodec {
		// we do not need to decompress or decode
		rw.w = &envelopingWriter{op: rw.op, w: rw.delegate}
	} else {
		rw.w = &transformingWriter{rw: rw, msg: &respMsg, w: rw.delegate}
	}
}

func (rw *responseWriter) Flush() {
	// We expose this method so server can call it and won't panic
	// or blow-up when doing type conversion. But it's a no-op
	// since we automatically flush at message boundaries when
	// transforming the response body.
}

func (rw *responseWriter) reportError(err error) {
	var end responseEnd
	if errors.As(err, &end.err) {
		end.httpCode = httpStatusCodeFromRPC(end.err.Code())
	} else {
		// TODO: maybe this should be CodeUnknown instead?
		end.err = connect.NewError(connect.CodeInternal, err)
		end.httpCode = http.StatusBadGateway
	}
	rw.reportEnd(&end)
}

func (rw *responseWriter) reportEnd(end *responseEnd) {
	if rw.endWritten {
		// ruh-roh... this should not happen
		return
	}
	switch {
	case rw.headersFlushed:
		// write error to body or trailers
		trailers := rw.op.client.protocol.encodeEnd(rw.op.client.codec, end, rw.delegate, false)
		if len(trailers) > 0 {
			httpMergeTrailers(rw.Header(), trailers)
		}
		rw.endWritten = true
	case rw.respMeta != nil:
		rw.respMeta.end = end
		rw.flushHeaders()
	default:
		rw.respMeta = &responseMeta{end: end}
		rw.flushHeaders()
	}
	// response is done
	rw.op.cancel()
	rw.err = context.Canceled
}

func (rw *responseWriter) flushHeaders() {
	if rw.headersFlushed {
		return // already flushed
	}
	cliRespMeta := *rw.respMeta
	cliRespMeta.codec = rw.op.client.codec.Name()
	cliRespMeta.compression = rw.op.client.reqCompression.Name()
	cliRespMeta.acceptCompression = intersect(rw.respMeta.acceptCompression, rw.op.canDecompress)
	hdr := rw.Header()
	statusCode := rw.op.client.protocol.addProtocolResponseHeaders(cliRespMeta, hdr)
	rw.delegate.WriteHeader(statusCode)
	if rw.respMeta.end != nil {
		// response is done
		trl := rw.op.client.protocol.encodeEnd(rw.op.client.codec, rw.respMeta.end, rw.delegate, true)
		httpMergeTrailers(hdr, trl)
		rw.endWritten = true
		rw.err = context.Canceled
	}
	rw.headersFlushed = true
}

func (rw *responseWriter) close() {
	if !rw.headersWritten {
		// treat as empty successful response
		rw.WriteHeader(http.StatusOK)
	}
	if rw.w != nil {
		_ = rw.w.Close()
	}
	rw.flushHeaders()
	if rw.endWritten {
		return // all done
	}
	end, err := rw.op.server.protocol.extractEndFromTrailers(rw.op, httpExtractTrailers(rw.Header()))
	if err != nil {
		end = responseEnd{
			err: connect.NewError(connect.CodeInternal, err),
		}
	}
	rw.reportEnd(&end)
}

// envelopingWriter will translate between envelope styles as data is
// written. It does not do any decompressing or deserializing of data.
type envelopingWriter struct {
	op *operation
	w  io.Writer
}

func (ew envelopingWriter) Write(data []byte) (int, error) {
	//TODO implement me
	panic("implement me")
}

func (ew envelopingWriter) Close() error {
	//TODO implement me
	panic("implement me")
}

// transformingWriter transforms the data from the original response
// into a new protocol form as the data is written. It must decompress
// and deserialize each message and then re-serialize (and optionally
// recompress) each message. Since the original incoming protocol may
// have different envelope conventions than the outgoing protocol, it
// also rewrites envelopes.
type transformingWriter struct {
	rw  *responseWriter
	msg *message
	w   io.Writer

	err             error
	buffer          *bytes.Buffer
	expectingBytes  int
	readingEnvelope bool
	latestEnvelope  envelope
}

func (tw *transformingWriter) Write(data []byte) (int, error) {
	if tw.err != nil {
		return 0, tw.err
	}
	if tw.buffer == nil {
		tw.reset()
	}

	if tw.expectingBytes == -1 {
		// TODO: implement a buffer size limit
		return tw.buffer.Write(data)
	}

	var writeCount int
	// For enveloped protocols, it's possible that data contains
	// multiple messages, so we need to process in a loop.
	for {
		remainingBytes := tw.expectingBytes - tw.buffer.Len()
		if len(data) < remainingBytes {
			tw.buffer.Write(data)
			writeCount += len(data)
			break
		}
		current := data[:remainingBytes]
		tw.buffer.Write(current)
		writeCount += remainingBytes
		data = data[remainingBytes:]
		if tw.readingEnvelope {
			var envBytes envelopeBytes
			_, _ = tw.buffer.Read(envBytes[:])
			var err error
			tw.latestEnvelope, err = tw.rw.op.serverEnveloper.decodeEnvelope(envBytes)
			if err != nil {
				tw.rw.reportError(err)
				tw.err = err
				return writeCount, err
			}
			// TODO: implement a buffer size limit
			tw.buffer = tw.msg.reset(tw.rw.op.bufferPool, false, tw.latestEnvelope.compressed)
			tw.expectingBytes = int(tw.latestEnvelope.length)
			tw.readingEnvelope = false
		} else {
			if err := tw.flushMessage(); err != nil {
				tw.rw.reportError(err)
				tw.err = err
				return writeCount, err
			}
			tw.expectingBytes = envelopeLen
			tw.readingEnvelope = true
		}
	}
	return writeCount, nil
}

func (tw *transformingWriter) Close() error {
	if tw.expectingBytes == -1 {
		if err := tw.flushMessage(); err != nil {
			tw.rw.reportError(err)
		}
	} else if tw.buffer != nil && tw.buffer.Len() > 0 {
		// Unfinished body!
		if tw.readingEnvelope {
			tw.rw.reportError(fmt.Errorf("handler only wrote %d out of %d bytes of message envelope", tw.buffer.Len(), envelopeLen))
		} else {
			tw.rw.reportError(fmt.Errorf("handler only wrote %d out of %d bytes of message", tw.buffer.Len(), tw.expectingBytes))
		}
	}
	tw.msg.release(tw.rw.op.bufferPool)
	tw.buffer = nil
	tw.err = errors.New("body is closed")
	return nil
}

func (tw *transformingWriter) flushMessage() error {
	if tw.latestEnvelope.trailer {
		data := tw.buffer
		if tw.latestEnvelope.compressed {
			data = tw.rw.op.bufferPool.Get()
			defer tw.rw.op.bufferPool.Put(data)
			if err := tw.rw.op.respCompression.decompress(data, tw.buffer); err != nil {
				return err
			}
		}
		end, err := tw.rw.op.serverEnveloper.decodeEndFromMessage(tw.rw.op.server.codec, data)
		if err != nil {
			return err
		}
		end.wasCompressed = tw.latestEnvelope.compressed
		tw.rw.reportEnd(&end)
		tw.err = errors.New("final message already written")
		return nil
	}

	// We've finished reading the message, so we can manually set the stage
	tw.msg.stage = stageRead
	if err := tw.msg.advanceToStage(tw.rw.op, stageSend); err != nil {
		return err
	}
	buffer := tw.msg.sendBuffer()
	if enveloper := tw.rw.op.clientEnveloper; enveloper != nil {
		env := envelope{
			compressed: tw.msg.wasCompressed,
			length:     uint32(buffer.Len()),
		}
		envBytes := enveloper.encodeEnvelope(env)
		if _, err := tw.w.Write(envBytes[:]); err != nil {
			return err
		}
	}
	if _, err := buffer.WriteTo(tw.w); err != nil {
		return err
	}
	// flush after each message
	tw.rw.flusher.Flush()

	tw.reset()
	return nil
}

func (tw *transformingWriter) reset() {
	if tw.rw.op.serverEnveloper != nil {
		tw.buffer = tw.msg.reset(tw.rw.op.bufferPool, false, false)
		tw.expectingBytes = envelopeLen
		tw.readingEnvelope = true
	} else {
		tw.buffer = tw.msg.reset(tw.rw.op.bufferPool, false, true)
		tw.expectingBytes = -1
	}
}

type errorWriter struct {
	rw          *responseWriter
	respMeta    *responseMeta
	processBody responseEndUnmarshaler
	buffer      *bytes.Buffer
}

func (ew *errorWriter) Write(data []byte) (int, error) {
	if ew.buffer == nil {
		return 0, errors.New("writer already closed")
	}
	// TODO: limit on size of the error body and how much we'll buffer?
	return ew.buffer.Write(data)
}

func (ew *errorWriter) Close() error {
	if ew.respMeta.end == nil {
		ew.respMeta.end = &responseEnd{}
	}
	bufferPool := ew.rw.op.bufferPool
	defer bufferPool.Put(ew.buffer)
	body := ew.buffer
	if compressPool := ew.rw.op.respCompression; compressPool != nil {
		uncompressed := bufferPool.Get()
		defer bufferPool.Put(uncompressed)
		if err := compressPool.decompress(uncompressed, body); err != nil {
			// can't really just return an error; we have to encode the
			// error into the RPC response, so we populate respMeta.end
			if ew.respMeta.end.httpCode == 0 || ew.respMeta.end.httpCode == http.StatusOK {
				ew.respMeta.end.httpCode = http.StatusInternalServerError
			}
			ew.respMeta.end.err = connect.NewError(connect.CodeInternal, fmt.Errorf("failed to decompress body: %w", err))
			body = nil
		} else {
			body = uncompressed
		}
	}
	if body != nil {
		ew.processBody(ew.rw.op.server.codec, body, ew.respMeta.end)
	}
	ew.rw.flushHeaders()
	ew.buffer = nil
	return nil
}

type messageStage int

const (
	stageEmpty = messageStage(iota)
	// This is the stage of a message after the raw data has been read from the client
	// or written by the server handler.
	//
	// At this point either compressed or data fields of the message will be populated
	// (depending on whether message data was compressed or not).
	stageRead
	// This is the stage of a message after the data has been decompressed and decoded.
	//
	// The msg field of the message is usable at this point. The compressed and data
	// fields of the message will remain populated if their values can be re-used.
	stageDecoded
	// This is the stage of a message after it has been re-encoded and re-compressed
	// and is ready to send (to be read by server handler or to be written to client).
	//
	// Either compressed or data fields of the message will be populated (depending on
	// whether message data was compressed or not).
	stageSend
)

func (s messageStage) String() string {
	switch s {
	case stageEmpty:
		return "empty"
	case stageRead:
		return "read"
	case stageDecoded:
		return "decoded"
	case stageSend:
		return "send"
	default:
		return "unknown"
	}
}

// message represents a single message in an RPC stream. It can be re-used in a stream,
// so we only allocate one and then re-use it for subsequent messages (if stream has
// more than one).
type message struct {
	// true if this is a request message read from the client; false if
	// this is a response message written by the server.
	isRequest bool

	// flags indicating if compressed and data should be preserved after use.
	sameCompression, sameCodec bool
	// wasCompressed is true if the data was originally compressed; this can
	// be false in a stream when the stream envelope's compressed bit is unset.
	wasCompressed bool

	stage messageStage

	// compressed is the compressed bytes; may be nil if the contents have
	// already been decompressed into the data field.
	compressed *bytes.Buffer
	// data is the serialized but uncompressed bytes; may be nil if the
	// contents have not yet been decompressed or have been de-serialized
	// into the msg field.
	data *bytes.Buffer
	// msg is the plain message; not valid unless stage is stageDecoded
	msg proto.Message
}

// sendBuffer returns the buffer to use to read message data to be sent.
func (m *message) sendBuffer() *bytes.Buffer {
	if m.stage != stageSend {
		return nil
	}
	if m.wasCompressed {
		return m.compressed
	}
	return m.data
}

// release releases all buffers associated with message to the given pool.
func (m *message) release(pool *bufferPool) {
	if m.compressed != nil {
		pool.Put(m.compressed)
	}
	if m.data != nil && m.data != m.compressed {
		pool.Put(m.data)
	}
	m.data, m.compressed, m.msg = nil, nil, nil
}

// reset arranges for message to be re-used by making sure it has
// a compressed buffer that is ready to accept bytes and no data
// buffer.
func (m *message) reset(pool *bufferPool, isRequest, isCompressed bool) *bytes.Buffer {
	m.stage = stageEmpty
	m.isRequest = isRequest
	m.wasCompressed = isCompressed
	// we only need one buffer to start, so put
	// a non-nil buffer into buffer1 and if we
	// have a second non-nil buffer, release it
	buffer1, buffer2 := m.compressed, m.data
	if buffer1 == nil && buffer2 != nil {
		buffer1, buffer2 = buffer2, buffer1
	}
	if buffer2 != nil && buffer2 != buffer1 {
		pool.Put(buffer2)
	}
	if buffer1 == nil {
		buffer1 = pool.Get()
	} else {
		buffer1.Reset()
	}
	if isCompressed {
		m.compressed, m.data = buffer1, nil
	} else {
		m.data, m.compressed = buffer1, nil
	}
	return buffer1
}

func (m *message) advanceToStage(op *operation, newStage messageStage) error {
	if m.stage == stageEmpty {
		return errors.New("message has not yet been read")
	}
	if m.stage > newStage {
		return fmt.Errorf("cannot advance message stage backwards: stage %v > target %v", m.stage, newStage)
	}
	if newStage == m.stage {
		return nil // no-op
	}

	if newStage == stageSend && m.sameCodec &&
		(!m.wasCompressed || (m.wasCompressed && m.sameCompression)) {
		// We can re-use existing buffer; no more action to take.
		m.stage = newStage
		return nil // no more action to take
	}

	switch {
	case m.stage == stageRead && newStage == stageSend:
		if !m.sameCodec {
			// If the codec is different we have to fully decode the message and
			// then fully re-encode.
			if err := m.advanceToStage(op, stageDecoded); err != nil {
				return err
			}
			return m.advanceToStage(op, newStage)
		}

		// We must de-compress and re-compress the data.
		if err := m.decompress(op, false); err != nil {
			return err
		}
		if err := m.compress(op); err != nil {
			return err
		}
		m.stage = newStage
		return nil

	case m.stage == stageRead && newStage == stageDecoded:
		if m.wasCompressed {
			if err := m.decompress(op, m.sameCompression && m.sameCodec); err != nil {
				return err
			}
		}
		if err := m.decode(op, m.sameCodec); err != nil {
			return err
		}
		m.stage = newStage
		return nil

	case m.stage == stageDecoded && newStage == stageSend:
		if !m.sameCodec {
			// re-encode
			if err := m.encode(op); err != nil {
				return err
			}
		}
		if m.wasCompressed {
			// re-compress
			if err := m.compress(op); err != nil {
				return err
			}
		}
		m.stage = newStage
		return nil

	default:
		return fmt.Errorf("unknown stage transition: stage %v to target %v", m.stage, newStage)
	}
}

// decompress will decompress data in m.compressed into m.data,
// acquiring a new buffer from op's bufferPool if necessary.
// If saveBuffer is true, m.compressed will be unmodified on
// return; otherwise, the buffer will be released to op's
// bufferPool and the field set to nil.
//
// This method should not be called directly as the message's
// buffers could get out of sync with its stage. It should
// only be called from m.advanceToStage.
func (m *message) decompress(op *operation, saveBuffer bool) error {
	var pool *compressionPool
	if m.isRequest {
		pool = op.client.reqCompression
	} else {
		pool = op.respCompression
	}
	if pool == nil {
		// identity compression, so nothing to do
		m.data = m.compressed
		if !saveBuffer {
			m.compressed = nil
		}
		return nil
	}

	var src *bytes.Buffer
	if saveBuffer {
		// we allocate a new buffer, but not the underlying byte slice
		// (it's cheaper than re-compressing later)
		src = bytes.NewBuffer(m.compressed.Bytes())
	} else {
		src = m.compressed
	}
	m.data = op.bufferPool.Get()
	if err := pool.decompress(m.data, src); err != nil {
		return err
	}
	if !saveBuffer {
		op.bufferPool.Put(m.compressed)
		m.compressed = nil
	}
	return nil
}

// compress will compress data in m.data into m.compressed,
// acquiring a new buffer from op's bufferPool if necessary.
//
// This method should not be called directly as the message's
// buffers could get out of sync with its stage. It should
// only be called from m.advanceToStage.
func (m *message) compress(op *operation) error {
	var pool *compressionPool
	if m.isRequest {
		pool = op.server.reqCompression
	} else {
		pool = op.respCompression
	}
	if pool == nil {
		// identity compression, so nothing to do
		m.compressed = m.data
		m.data = nil
		return nil
	}

	m.compressed = op.bufferPool.Get()
	if err := pool.compress(m.compressed, m.data); err != nil {
		return err
	}
	op.bufferPool.Put(m.data)
	m.data = nil
	return nil
}

// decode will unmarshal data in m.data into m.msg. If
// saveBuffer is true, m.data will be unmodified on return;
// otherwise, the buffer will be released to op's bufferPool
// and the field set to nil.
//
// This method should not be called directly as the message's
// buffers could get out of sync with its stage. It should
// only be called from m.advanceToStage.
func (m *message) decode(op *operation, saveBuffer bool) error {
	switch {
	case m.isRequest && op.clientReqNeedsPrep:
		return op.clientPreparer.prepareUnmarshalledRequest(op, m.data.Bytes(), m.msg)
	case !m.isRequest && op.serverRespNeedsPrep:
		return op.serverPreparer.prepareUnmarshalledResponse(op, m.data.Bytes(), m.msg)
	}

	var codec Codec
	if m.isRequest {
		codec = op.client.codec
	} else {
		codec = op.server.codec
	}

	if err := codec.Unmarshal(m.data.Bytes(), m.msg); err != nil {
		return err
	}
	if !saveBuffer {
		op.bufferPool.Put(m.data)
		m.data = nil
	}
	return nil
}

// encode will marshal data in m.msg into m.data.
//
// This method should not be called directly as the message's
// buffers could get out of sync with its stage. It should
// only be called from m.advanceToStage.
func (m *message) encode(op *operation) error {
	buf := op.bufferPool.Get()
	var data []byte
	var err error

	switch {
	case m.isRequest && op.serverReqNeedsPrep:
		data, err = op.serverPreparer.prepareMarshalledRequest(op, buf.Bytes(), m.msg, op.request.Header)
	case !m.isRequest && op.clientRespNeedsPrep:
		data, err = op.clientPreparer.prepareMarshalledResponse(op, buf.Bytes(), m.msg, op.writer.Header())
	default:
		var codec Codec
		if m.isRequest {
			codec = op.server.codec
		} else {
			codec = op.client.codec
		}
		data, err = codec.MarshalAppend(buf.Bytes(), m.msg)
	}

	if err != nil {
		op.bufferPool.Put(buf)
		m.data = nil
		return err
	}
	m.data = op.bufferPool.Wrap(data, buf)
	return nil
}

func intersect(setA, setB []string) []string {
	length := len(setA)
	if len(setB) < length {
		length = len(setB)
	}
	if length == 0 {
		// If either set is empty, the intersection is empty.
		// We don't use nil since it is used in places as a sentinel.
		return make([]string, 0)
	}
	result := make([]string, 0, length)
	for _, item := range setA {
		for _, other := range setB {
			if other == item {
				result = append(result, item)
				break
			}
		}
	}
	return result
}
